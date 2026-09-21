<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine Proxy

A discovery-aware HTTP reverse proxy that fronts every enabled inference engine.
It runs no mDNS browse of its own: routing targets come from the broker's
discovery relay (each facade sends `discovery:subscribe` for its engine's
service key and replaces its routing overlay from each pushed `discovery:nodes`
snapshot) plus user-added manual nodes. It forwards HTTP requests to the
selected node, aggregates the model-list routes across candidate nodes, and
exposes a bidirectional JSON-RPC 2.0 control channel over stdio (or an IPC
socket).

**One process, one facade per engine.** The process starts with no engine and no
listener; the broker asks for each engine's facade with `facade/enable`, which
carries that engine's port and alias addresses. A flag cannot express this,
because the broker plans a different port for each engine.

They share a process on purpose. Between scheduler snapshots a facade takes
short-lived reservations for work it has dispatched, and those live in the
process — a process per engine split that picture, so simultaneous bursts on
both engines could pick the same node believing it idle.

The cost is shared fate for the **process**: a crash takes every facade down and
the supervisor restarts them together. Smaller failures are contained to one
engine — a lost bind race, a port that cannot be re-listened, or a panic while
handling a request. Each of those used to end the process, which was fine when
the process was one engine.

`spec.md` is normative; where it and this file disagree, it wins. Everything
below is shared behavior unless a section says otherwise.

## Build

```bash
go build -o nvpair-proxy .
```

## Usage

```
nvpair-proxy [flags]
```

Then, over the control channel, for each engine to front:

```json
{"jsonrpc":"2.0","id":1,"method":"facade/enable",
 "params":{"engine":"ollama","port":11434,"ignorePersistedPort":true}}
```

The response is `{"engine":"ollama","port":11434}`, reporting the port actually
bound — enable is a request, and the child's persisted-port restore can override
the port asked for. Enabling an engine that is already up is an idempotent
success, so a redelivered enable never tears down a working listener.

### Flags

Only process-scoped settings are flags. Anything per-engine is a `facade/enable`
parameter, because one flag cannot carry two engines' plans.

| Flag | Default | Description |
|------|---------|-------------|
| `--ipc` | *(empty — use stdio)* | Path to a Unix domain socket or Windows named pipe for IPC |
| `--cluster-dir` | *(empty)* | Cluster trust directory (`node.crt`/`node.key` plus trusted pins). Enables the LAN mTLS inference ingress while this node is a cluster member; empty means no ingress and no peer candidates. |
| `--log-level` | *(`$NVPAIR_LOG_LEVEL`, else `info`)* | Initial log level: `debug`, `info`, `warn`, or `error`. Changeable at runtime with `log/set-level`. |
| `--version` | | Print version and exit |

### `facade/enable` parameters

| Field | Default | Description |
|-------|---------|-------------|
| `engine` | *(required)* | Which engine to front: `ollama` or `lmstudio`. An unknown name is rejected with the accepted values. |
| `port` | per engine, see below | HTTP listen port for request forwarding. Must be 1–65535, or omitted for the engine's standalone default. `0` means "the default" rather than "pick an ephemeral port", and any other out-of-range value is rejected, because the facade announces the requested port in its `ready` notification and the broker would be told `0`. |
| `aliasAddresses` | *(empty)* | Optional secondary `host:port` values for the same routing handler, one per loopback family so `localhost` resolves either way. Only literal loopback addresses are accepted; the broker uses this for a safe inherited local `OLLAMA_HOST`, and the aliases are not advertised to peers. Accepted only for an engine with an inherited host variable — today Ollama alone — and rejected for any other. |
| `ignorePersistedPort` | `false` | Use `port` even when a saved port exists (used by broker-managed startup) |

### What differs per engine

Everything engine-specific is one entry in `engines.go`, plus the shared
identity in `nvpair-shared/engines`.

| | `"engine":"ollama"` | `"engine":"lmstudio"` |
|---|---|---|
| Facade id — error-ID prefix, broker relay namespace, TUI proxies-view tab | `ollama-proxy` | `lmstudio-proxy` |
| Discovery service key | `ol` | `lm` |

The **log component, supervisor label, and TUI health crash key are not in that
table**: they name the process (`nvpair-proxy`), not a facade, because one
process hosts every engine and there is no single engine to attribute them to.
Facade-scoped log records carry an `engine` field instead. The supervisor label
and the health crash key are matched against each other, so they move together
— see `nvpair-shared/engines` and `spec.md` §9.

| | `"engine":"ollama"` | `"engine":"lmstudio"` |
|---|---|---|
| Engine's own client-facing port | 11434 | 1234 |
| Where PAIR relocates the engine | 11435 | 1235 |
| Standalone port, used when `port` is omitted | 11435 | 1234 |
| Persisted-port file (declared, not derived) | `proxy-port.json` | `lmstudio-proxy-port.json` |
| Model-list routes | `GET /api/tags` (native), `GET /v1/models` (OpenAI) | `GET /v1/models` (OpenAI) |
| Inference routes | `/api/generate`, `/api/chat`, `/api/embeddings`, `/api/embed`, plus the OpenAI set | `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` |
| Model naming | untagged means `:latest`, so `llama3` and `llama3:latest` are one model | identifiers compared byte for byte |

The route table is a **classifier, not an allowlist**. An unlisted path is
forwarded verbatim, which is how `/api/show`, `/api/pull`, `/api/ps`,
`/api/version` and `OPTIONS` preflights keep working.

### HTTP Reverse Proxy

A facade listens on its enabled port and forwards incoming requests to the
currently active node — except the model-list routes, which are queried across
every candidate node concurrently and merged into one de-duplicated inventory.
Point your client at the proxy and it handles routing.

When the broker supplies `aliasAddresses`, the facade reserves that
loopback-only endpoint before reporting ready and serves it through the same
routing and workload-lifecycle handler. A `localhost` alias reserves `127.0.0.1`
and `::1` atomically so client resolution cannot bypass the router. An occupied
alias is non-fatal: its existing owner is untouched and the proxy reports an
actionable warning while the primary listener stays available.

**Cluster ingress.** The listener carries two personalities, demultiplexed by
each connection's first byte. Plaintext HTTP is accepted only from loopback; a
LAN caller is refused. When `--cluster-dir` shows this node is a cluster member,
the same listener also terminates cluster mTLS: a peer whose client certificate
matches one of this node's pins is forwarded straight to the local engine
reported by `node/set-local-backend`, and is never re-routed onward to another
node. Membership and pins are re-derived per request, so joining or leaving a
cluster needs no restart.

**Persisted port.** A port chosen at runtime via `set-port` is saved to the
per-user data dir (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on
Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux) and
**restored when the facade is enabled**, taking precedence over the requested
`port`/the default — so the proxy comes back up where it was last put. Each
engine has its own file, so moving one proxy never moves another.
`ignorePersistedPort` deliberately bypasses that restoration for a
broker-coordinated start. Delete the file (or
`set-port` back to the default) to revert.

LM Studio additionally refuses to restore port 1235 even if it is stored: that
is where engine-manager runs a managed LM Studio, so a proxy restoring it would
sit on the engine's own port. The stored value predates the current default.

**Browser clients (CORS).** The proxy is usable from a web front end. When an
engine is available, the proxy forwards an `OPTIONS` preflight so an
engine-declared exact origin and credentials policy reaches the browser
unchanged. If no engine is available, the engine returns no CORS policy, or a
non-loopback caller must be refused before routing, the proxy answers with its
own permissive `204` fallback. It also labels every response it generates —
including rejections such as the `502` when no node is available and the `403`
refusing a non-loopback plaintext caller — with `Access-Control-Allow-Origin: *`.
That matters as much as the success path: a response without those headers
reaches the browser as a generic "CORS error" with the real status and reason
stripped out, so the caller cannot tell what went wrong. A locally answered
preflight grants no access, since the request that follows still faces the same
gate. The policy comes from `nvpair-shared/cors`, so every engine answers
identically.

A response *forwarded from an engine* is different: if the engine set its own
`Access-Control-Allow-Origin` (Ollama with `OLLAMA_ORIGINS`), that header is
passed through untouched rather than replaced with the wildcard, so a
deliberately narrow engine policy is never silently widened and a credentialed
response is not broken. An engine that sends no CORS header has expressed no
policy to keep, so the proxy supplies its own — and drops any
`Access-Control-Allow-Credentials` that arrived without an origin, because a
browser rejects that header alongside a wildcard origin and would discard the
response the fallback exists to make readable.

One limit is outside the proxy's control: current Chromium-based browsers gate a
request from a public origin to a local or loopback address behind the user's
[Local Network Access](https://chromestatus.com/feature/5152728072060928)
permission, which replaced the old server-side opt-in header. No header the
proxy sends can grant that. A hosted page needs the permission plus a
`fetch(url, { targetAddressSpace: 'loopback' })` annotation; a page served from
the local machine is unaffected.

### Node selection

- **Eligibility**: Before routing model-bearing inference, the proxy keeps only
  nodes whose current inventory for this engine advertises the requested model,
  compared using the engine's naming rule. An empty or non-matching inventory is
  excluded until a later discovery update; if no advertised owner is routable,
  the proxy returns a local `502`.
- **Auto**: When no eligible node is explicitly selected, the proxy follows
  `node/set-priority`, then discovered nodes in stable ID order.
- **Priority (scheduler-driven)**: The Job Scheduler ranks the cluster
  least-loaded-first by pending workload plus smoothed GPU pressure and, via
  `nvpair-ui-broker`, pushes the ordered node list with those per-node counts to
  this proxy with `node/set-priority`. Auto routing sends the request to the
  listed node carrying the least estimated load. See
  [`nvpair-job-scheduler`](../nvpair-job-scheduler/README.md).
- **Manual**: `node/select` pins traffic to a specific node. A manual pin
  **overrides the priority list only when that node is eligible** for the
  requested model.
- **Failover**: If the selected node disappears from the discovery set, the
  proxy falls back to auto-select and emits `node/selection-changed`. A
  transport error or retryable status, including a model `404` from an
  advertised owner with stale inventory, steps to the next eligible owner.

### IPC Transport

By default the proxy communicates over **stdin/stdout** using newline-delimited
JSON-RPC 2.0 (one message per line). All diagnostic logging goes to **stderr**.

For environments where stdout may conflict with the host process (e.g. Electron),
pass `--ipc` to redirect the JSON-RPC channel to a named socket or pipe. The
parent process should create and listen on the endpoint before spawning the
proxy.

```bash
# Default: stdin/stdout
nvpair-proxy

# Unix domain socket
nvpair-proxy --ipc /tmp/nvpair-proxy.sock

# Windows named pipe
nvpair-proxy --ipc \\.\pipe\nvpair-proxy
```

## JSON-RPC 2.0 Protocol

The control channel is identical for every engine; only the broker's relay
prefix differs. The generated method surface — every request and notification,
with payload shapes — is in
[`desktop/docs/services-api.md`](../../desktop/docs/services-api.md), produced by
`npm --prefix desktop run service-contracts:write`. Read that rather than a
hand-maintained copy, which is how the two per-engine READMEs drifted.

The proxy emits `ready` on startup carrying its bound port, which is the
authoritative source of where it is listening — do not assume a port before it
reports one.

## Shutdown

Closing stdin (observed as EOF) drains the HTTP server and exits. The broker
owns the lifecycle: `shutdown` relayed from a client is refused.

## Adding an engine

Add one entry to `nvpair-shared/engines` for the cross-process identity, one to
`engines.go` here for the proxy-only values, and one to the broker's
`engineproxy.go` for its ownership. See the "Adding a new engine" section of the
proxy-unification plan for the full checklist, including the one-liners that are
still per-engine (a `noderec.ServiceKey`, the TUI health label, the desktop
engine-type constants, and an engine-manager manifest).
