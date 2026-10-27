//go:build windows

package plugins

// monotonicNS has no clock to share with a plugin on Windows: commands carry deadline_ms alone (counted down to the
// moment the line is written), and expires_at_mono_ns is left out.
func monotonicNS() (int64, bool) { return 0, false }
