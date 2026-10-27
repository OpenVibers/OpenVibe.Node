//go:build !linux

package main

import (
	"errors"
	"os"
)

// inheritedDeskPipe: desk sandboxes are Linux's (namespaces, cgroup v2); fds 3 and 4 are never touched elsewhere.
func inheritedDeskPipe() (in, out *os.File, err error) {
	return nil, nil, errors.New("desk sandboxes need Linux")
}
