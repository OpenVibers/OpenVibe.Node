//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// inheritedDeskPipe answers fds 3 and 4 as the caller (the computer plugin) passed them, the browser's
// --remote-debugging-pipe. Only a pipe or a socket is taken: in a process started without them those numbers are the
// Go runtime's own (its poller), and an *os.File around one would close it when collected.
func inheritedDeskPipe() (in, out *os.File, err error) {
	for _, fd := range []int{3, 4} {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return nil, nil, fmt.Errorf("fd %d: %w (fds 3 and 4 are the browser's pipe)", fd, err)
		}
		if t := st.Mode & syscall.S_IFMT; t != syscall.S_IFIFO && t != syscall.S_IFSOCK {
			return nil, nil, fmt.Errorf("fd %d is not a pipe (fds 3 and 4 are the browser's pipe)", fd)
		}
	}
	return os.NewFile(3, "desk in"), os.NewFile(4, "desk out"), nil
}
