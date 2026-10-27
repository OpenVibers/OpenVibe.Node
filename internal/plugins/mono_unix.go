//go:build !windows

package plugins

import "golang.org/x/sys/unix"

// monotonicNS reads CLOCK_MONOTONIC: one clock for every process on this machine (a plugin reads the same one, in
// Python time.clock_gettime_ns(time.CLOCK_MONOTONIC)), which a step of the wall clock does not move.
func monotonicNS() (int64, bool) {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0, false
	}
	return ts.Nano(), true
}
