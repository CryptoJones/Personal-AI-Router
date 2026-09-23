// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package ingressauth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"syscall"
)

// checkKeyFileAccess refuses a key file that another account could read or
// change, the way sshd treats authorized_keys: a private mode is not enough if
// someone else owns the file and can change its contents or mode at will. The
// file must belong to the proxy's user or to root, since an administrator may
// provision it for a service user, and must grant nothing to others. A
// FileInfo without Unix ownership data is refused too: on a Unix-like system
// that is not a file the gate can vouch for.
//
// Group read is refused except in one shape: a root-owned file, read-only to a
// group the proxy itself belongs to (0440 or 0640). That is how Kubernetes
// mounts a Secret under a pod fsGroup, where the group exists for this
// workload alone. The file must be root's so a user cannot open their own key
// to a shared login group, such as macOS "staff", on the proxy's behalf.
func checkKeyFileAccess(info fs.FileInfo) error {
	return checkKeyFileAccessAs(info, os.Geteuid(), processGroups())
}

func checkKeyFileAccessAs(info fs.FileInfo, euid int, groups []int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine the key file's owner")
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if uid != euid && uid != 0 {
		return fmt.Errorf("owned by uid %d, not by the proxy's user (uid %d)", uid, euid)
	}
	perm := info.Mode().Perm()
	switch {
	case perm&0o077 == 0:
		return nil
	case perm&0o037 == 0 && uid == 0 && slices.Contains(groups, gid):
		return nil
	case perm&0o007 != 0:
		return fmt.Errorf("permissions %04o allow other users to read it; chmod 600", perm)
	default:
		return fmt.Errorf("permissions %04o allow group %d to access it; chmod 600, or make it root-owned, group read-only (0440), and in a group the proxy belongs to", perm, gid)
	}
}

// processGroups is the proxy's effective and supplementary group IDs. A
// failure to list the supplementary ones leaves just the effective group,
// which only narrows what checkKeyFileAccess accepts.
func processGroups() []int {
	groups, _ := os.Getgroups()
	return append(groups, os.Getegid())
}
