package plugins

import "github.com/OpenVibers/OpenVibe.Node/internal/mono"

// The clock a plugin shares with the core (internal/mono: CLOCK_MONOTONIC on Unix, the raw QueryPerformanceCounter on
// Windows, named in hello as shared_clock): a command's expires_at_mono_ns is an instant on it, so the runtime can take
// off the time a line spent in the pipe and its reader (Maticus, 2026-10-27).

// monotonicNS reads it; a variable so tests can take it away (a machine without one, or a read that fails).
var monotonicNS = mono.Read

// sharedClockName is the clock hello names.
const sharedClockName = mono.Name
