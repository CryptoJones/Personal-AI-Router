<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Engine Proxy (`nvpair-proxy`)

> **Status: implemented.** House spec style (`nvpair-cluster-manager`,
> `nvpair-workload-manager`, `nvpair-engine-manager`, `nvpair-job-scheduler`).
> Where this file and `README.md` disagree, this file wins.

## 1. Purpose

Carries inference **to** the node that should serve it. One process hosts a
**facade** per enabled engine: each facade is an HTTP reverse proxy speaking its
engine's own dialect, filtering to nodes that advertise the requested model,
choosing among them, and failing over when one refuses.

It is a data plane, not a policy component. Ranking belongs to
`nvpair-job-scheduler`, membership and trust to `nvpair-cluster-manager`,
lifecycle to `nvpair-engine-manager`. What this process owns is the choice of
*which advertised owner receives this request*, and the local accounting that
makes that choice good under bursts.

## 2. Scope

**In scope**

- One listener per enabled engine, serving loopback plaintext for local clients
  and pin-gated cluster mTLS for peers on the same port.
- Model-owner filtering, target precedence, and retryable failover.
- Per-request workload lifecycle events.
- The process-wide reservation map that makes concurrent dispatch spread.
- The persisted per-engine port a user chose through `set-port`.

**Out of scope**

- Deciding node rank (scheduler), membership or pinning (cluster-manager),
  engine install/start/stop (engine-manager).
- Any cryptography of its own: it consumes the trust fabric, never builds it.
- Model search or catalog browsing.

## 3. Process model

The process starts with **no engine and no listener**. The broker then sends one
`facade/enable` per engine, carrying that engine's port and any alias addresses.

A flag cannot express this. The broker plans a different port for each engine —
Ollama's managed facade wants `:11434` while LM Studio's wants `:1234`, and
either may be absent so the child keeps its own persisted port — and a
single-valued flag carries only one plan.

### 3.1 Why one process

Between scheduler snapshots a facade takes short-lived **reservations** for work
it has dispatched but that the scheduler has not yet observed. Those live in the
process. With a process per engine each held half the picture, so simultaneous
Ollama and LM Studio bursts could both pick the same node believing it idle.
Sharing the map is the reason the engines share a process.

### 3.2 What sharing costs

**Shared fate on the process.** A crash takes every facade down; the supervisor
restarts them together and the crash is reported once, against the process.

Inside the process the boundaries are finer, and deliberately so:

| Failure | Blast radius |
| --- | --- |
| Bind race lost on enable | That engine only; reported so the broker can retry elsewhere |
| Port cannot be re-listened | That engine only; its ownership gate is released |
| Panic handling a request | That request only; logged with stack, answered as an internal error |
| Process crash or exit | Every facade |

The first three used to be process-fatal, which was acceptable when the process
was one engine. They are contained now because they are not.

## 4. Addressing

Every facade-scoped message carries the engine it concerns, in both directions.
Without it the engine would be implied by which process a message travelled
through, which is precisely the property one process destroys.

The address is a bare engine id (`ollama:ready`), deliberately **not** the
component name (`ollama-proxy:ready`). The component form is how a client
addresses the component through the broker; the two meet in the broker's relay,
which takes one off and puts the other on.

| Class | Addressed? | Examples |
| --- | --- | --- |
| Facade-scoped | yes | `ready`, `error`, `node/*`, `errors:*`, `set-port`, `discovery:subscribe`, `discovery:nodes` |
| Process-scoped | no | `log/set-level`, `node/set-priority`, `workload:*`, `discovery:node-activity` |
| Bootstrap | names its engine in the payload | `facade/enable` |

`facade/enable` is unaddressed because it runs *before* the facade it names
exists. Only a known engine id counts as an address: plenty of unaddressed
methods contain a colon, so treating `errors:report` as engine `errors` would
strip a real method down to `report`.

An unaddressed facade-scoped message resolves to **no** facade. Falling back to
"the only facade" would work with one engine enabled and misroute silently with
two.

## 5. Request path

For a model-bearing inference request:

1. Filter a request-local discovery snapshot to nodes whose per-engine inventory
   advertises the requested model. Ollama normalizes the implicit `:latest` tag;
   LM Studio ids match exactly. An empty owner set returns a local `502` without
   contacting an engine.
2. Order the eligible owners: explicit `node/select` pin, then the scheduler's
   priority list, then deterministic default ordering.
3. Reserve the least estimated-loaded scheduler-listed candidate and move it to
   the front (§6).
4. Forward, failing over through the remaining candidates on a retryable status
   or transport error. An upstream model `404` counts as retryable, because
   positive inventory can be stale.

An ineligible manual selection cannot override the capability gate, and failover
never broadens to an excluded node.

## 6. Reservations

A reservation is one in-flight dispatch this process has made since the last
scheduler snapshot. Estimated load for a node is
`pending + gpuPressure + reservations`, the first two from the scheduler.

- **Taken** when a candidate is chosen, unless an eligible manual pin applies.
- **Moved** to the node a failover actually lands on, because the node that
  refused the request is not doing the work.
- **Released** when the request ends, so a node stops counting as loaded as soon
  as it stops working. Without release a trickle of short requests makes an idle
  node read as busy for the rest of the snapshot interval.

Each is stamped with the snapshot **generation** it was counted against. A
snapshot supersedes every reservation taken before it — its pending counts
already include that work — so a release naming an older generation is dropped.
Applying it would double-count the completion and drive the node's estimated
load below zero, which reads as permanently idle and attracts every subsequent
dispatch.

`node/set-priority` therefore carries a generation and is applied at most once;
a redelivered snapshot must not clear live reservations.

## 7. Listener personalities

One port per facade, demultiplexed on the connection's first byte:

- **Loopback plaintext** for local clients. A LAN caller is refused.
- **Cluster mTLS** when `--cluster-dir` shows this node is a member: a peer
  whose client certificate matches a local pin is forwarded straight to the
  local engine reported by `node/set-local-backend`, never re-routed onward.

Membership and pins are re-derived per request and on a watch, so joining or
leaving a cluster needs no restart.

An engine with an inherited host variable (today Ollama alone) may also be given
loopback-only **alias** addresses, so clients already using that variable enter
the same routing path. An occupied alias is non-fatal: the existing owner is
untouched, the primary listener stays up, and a warning is reported.

## 8. Ports

| | Ollama | LM Studio |
| --- | --- | --- |
| Engine's own client-facing port | 11434 | 1234 |
| Where PAIR relocates the engine | 11435 | 1235 |
| Standalone port, when `port` is omitted | 11435 | 1234 |
| Persisted-port file | `proxy-port.json` | `lmstudio-proxy-port.json` |

A port chosen at runtime via `set-port` is persisted per engine and restored
when that facade is enabled, taking precedence over the requested port, so the
facade returns where the user left it. `ignorePersistedPort` bypasses that for a
broker-coordinated start.

`port` 0 means "the engine's standalone default". Any other out-of-range value
is rejected rather than honoured as ephemeral: the facade announces the
requested port in `ready`, so binding an ephemeral one would leave the broker
unable to locate it.

## 9. Logging constraints

Never log prompts, chat messages, request bodies, stream chunks, full responses,
credentials, or pairing data. Operational metadata only — engine, model, job id,
node id, path, stream flag, normalized error text — and keep per-request routing
detail below info level.

The log component is the **process** (`nvpair-proxy`), because at process init
there is no engine to name. Facade-scoped records carry an `engine` field, which
is what keeps a line attributable when several facades share the sink.

## 10. Adding an engine

Everything engine-specific is one entry in `engines.go` plus the shared identity
in `nvpair-shared/engines`. A new engine needs: its identity and discovery
service key, its ports and persisted-port filename, its route table and model
normalization, and its empty model-list envelope. It needs no new process, no
new supervisor, and no new relay wiring.

## 11. Failure modes

| Mode | Behavior |
| --- | --- |
| Requested port taken at enable | Tagged bind failure; broker retries on a fallback port |
| No advertised owner for the model | Local `502`, no engine contacted |
| All owners refuse retryably | Last upstream status surfaced |
| Transport error with candidates left | Forget the node's confirmed address, fail over |
| Client disconnects mid-stream | Terminal workload event emitted at once; upstream cancelled |
| Broker link closed | Facades stop serving, then the shared transport pool closes |
