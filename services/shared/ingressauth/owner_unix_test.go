// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package ingressauth

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// fakeInfo is a fs.FileInfo whose mode and Sys() the test controls, so
// ownership cases that would otherwise need root (a file owned by someone
// else, or by root in a Kubernetes fsGroup) can be exercised.
type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) Sys() any          { return f.sys }

func TestCheckKeyFileAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte(keyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkKeyFileAccess(real); err != nil {
		t.Fatalf("a file this process just created is refused: %v", err)
	}

	const me, other, fsGroup, shared = 1000, 1001, 2000, 20
	groups := []int{shared, fsGroup}
	cases := []struct {
		name string
		mode fs.FileMode
		sys  any
		ok   bool
	}{
		{"owned by the process user", 0o600, &syscall.Stat_t{Uid: me}, true},
		{"owned by root", 0o400, &syscall.Stat_t{Uid: 0}, true},
		{"owned by another user", 0o600, &syscall.Stat_t{Uid: other}, false},
		{"world-readable", 0o604, &syscall.Stat_t{Uid: me}, false},
		{"Kubernetes Secret under fsGroup, 0440", 0o440, &syscall.Stat_t{Uid: 0, Gid: fsGroup}, true},
		{"Kubernetes Secret under fsGroup, 0640", 0o640, &syscall.Stat_t{Uid: 0, Gid: fsGroup}, true},
		{"root-owned, group not the proxy's", 0o440, &syscall.Stat_t{Uid: 0, Gid: 3000}, false},
		{"root-owned, group-writable", 0o460, &syscall.Stat_t{Uid: 0, Gid: fsGroup}, false},
		{"root-owned, group-executable", 0o450, &syscall.Stat_t{Uid: 0, Gid: fsGroup}, false},
		{"root-owned, group-readable and world-readable", 0o444, &syscall.Stat_t{Uid: 0, Gid: fsGroup}, false},
		{"user-owned, group-readable to a shared group", 0o640, &syscall.Stat_t{Uid: me, Gid: shared}, false},
		{"no ownership data", 0o600, nil, false},
		{"foreign Sys type", 0o600, struct{}{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKeyFileAccessAs(fakeInfo{FileInfo: real, mode: tc.mode, sys: tc.sys}, me, groups)
			if (err == nil) != tc.ok {
				t.Fatalf("checkKeyFileAccessAs err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
