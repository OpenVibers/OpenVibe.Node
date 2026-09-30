//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// chownLikeDir gives a file written by root (pair) to the account that owns its directory (the service account).
func chownLikeDir(path, dir string) {
	st, err := os.Stat(dir)
	if err != nil {
		return
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		_ = os.Chown(path, int(sys.Uid), int(sys.Gid))
	}
}

// chownTree hands the config and state directories to the service account.
func chownTree(name string, dirs ...string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("no account %q (create it first, e.g. with install.sh): %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, d := range dirs {
		err := filepath.Walk(d, func(p string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(p, uid, gid)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
