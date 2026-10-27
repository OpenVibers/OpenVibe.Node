// Package mono is the machine's precise monotonic clock, in nanoseconds. Every deadline on this machine is measured on
// it: in the core (the link's estimate of Bot's clock, its round trips, a command's receipt and the instant it
// expires) and, through expires_at_mono_ns, in the plugins the core starts. It is one clock for every process on the
// machine, which a step of the wall clock does not move: CLOCK_MONOTONIC on Linux and macOS (mono_unix.go), the raw
// QueryPerformanceCounter on Windows (mono_windows.go).
//
// It is not Go's own monotonic clock (Maticus, 2026-10-27). On Windows that one is the interrupt time, which moves once
// per clock tick: every 15.625 ms unless a program has raised the timer resolution, and Go no longer does since it
// sleeps on high-resolution timers. A deadline measured on it could look up to a tick further away than it is.
package mono

import "time"

// Instant is a reading of the clock: nanoseconds since an arbitrary point fixed at boot.
type Instant int64

// Read is the clock now, or ok false when it cannot be read: never on Linux or macOS, and never on Windows XP or later
// by Microsoft's documentation of QueryPerformanceCounter. Plugins are told the clock only when it can be read.
func Read() (int64, bool) { return read() }

// startNS and startGo are one reading of the clock and of Go's, taken together, for Now's fallback.
var (
	startNS, _ = read()
	startGo    = time.Now()
)

// Now is the clock now. Should a read ever fail, it is the first reading plus the time Go's clock has run since: the
// same timeline, at Go's resolution.
func Now() Instant {
	if ns, ok := read(); ok {
		return Instant(ns)
	}
	return Instant(startNS + int64(time.Since(startGo)))
}

// Since is how long ago i was; Until how long until it is.
func Since(i Instant) time.Duration { return time.Duration(Now() - i) }
func Until(i Instant) time.Duration { return time.Duration(i - Now()) }

func (i Instant) Add(d time.Duration) Instant { return i + Instant(d) }
func (i Instant) Sub(j Instant) time.Duration { return time.Duration(i - j) }

// qpcNS turns a QueryPerformanceCounter reading into nanoseconds, rounded down: whole seconds and the remainder apart,
// so nothing overflows int64 (ticks × 1e9 would after about 15 minutes of uptime at 10 MHz). The Python runtime does the
// same integer arithmetic on the same raw counter (openvibe_plugin._qpc_ns), so the two values compare exactly.
func qpcNS(ticks, freq int64) int64 {
	return (ticks/freq)*1_000_000_000 + (ticks%freq)*1_000_000_000/freq
}
