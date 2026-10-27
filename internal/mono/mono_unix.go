//go:build !windows

package mono

import "golang.org/x/sys/unix"

// Name is the clock as hello names it to a plugin (shared_clock): the plugin reads the same one, or says it cannot.
const Name = "clock_monotonic"

// read is CLOCK_MONOTONIC (a plugin reads it in Python with time.clock_gettime_ns(time.CLOCK_MONOTONIC)).
func read() (int64, bool) {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0, false
	}
	return ts.Nano(), true
}
