package mono

import (
	"runtime"
	"testing"
	"time"
)

// The same vectors as plugins/sdk/tests/test_strict_timing.py QPC_VECTORS: Node and the Python runtime turn a raw
// QueryPerformanceCounter reading into the same nanoseconds (rounded down, no overflow at a hundred years of uptime).
func TestQPCNSMatchesThePythonRuntime(t *testing.T) {
	for _, v := range [][3]int64{
		{0, 10_000_000, 0},
		{9_999_999, 10_000_000, 999_999_900},
		{30_000_005, 10_000_000, 3_000_000_500},
		{3_579_545*3600 + 1, 3_579_545, 3_600_000_000_279},
		{31_536_000_000_000_007, 10_000_000, 3_153_600_000_000_000_700},
		{2_441_406*86_400 + 2_441_405, 2_441_406, 86_400_999_999_590},
	} {
		if got := qpcNS(v[0], v[1]); got != v[2] {
			t.Errorf("qpcNS(%d, %d) = %d, want %d", v[0], v[1], got, v[2])
		}
	}
}

// Every OS has the clock (Windows: QueryPerformanceCounter, from v0.30.0), it never goes back, and Now reads it.
func TestTheClockIsThereAndMonotonic(t *testing.T) {
	a, ok := Read()
	if !ok {
		t.Fatalf("no clock on %s", runtime.GOOS)
	}
	n := Now()
	b, _ := Read()
	if Instant(a) > n || n > Instant(b) {
		t.Fatalf("Now is not the clock: %d ≤ %d ≤ %d does not hold", a, n, b)
	}
	want := map[string]string{"windows": "qpc"}[runtime.GOOS]
	if want == "" {
		want = "clock_monotonic"
	}
	if Name != want {
		t.Fatalf("the clock on %s is %q, want %q", runtime.GOOS, Name, want)
	}
}

// The clock resolves well under a millisecond on every OS, so no deadline is measured a clock tick long. On Windows
// Go's own monotonic clock is the interrupt time: the log line shows it moving a few times in the same window (or not
// at all), where this clock moves thousands of times.
func TestTheClockResolvesMicroseconds(t *testing.T) {
	distinct, goDistinct := 0, 0
	var last Instant
	var lastGo time.Duration
	t0, start := time.Now(), Now()
	for Since(start) < 5*time.Millisecond {
		if n := Now(); n != last {
			distinct, last = distinct+1, n
		}
		if g := time.Since(t0); g != lastGo {
			goDistinct, lastGo = goDistinct+1, g
		}
	}
	t.Logf("%s: %s took %d distinct readings in 5 ms; Go's monotonic clock %d", runtime.GOOS, Name, distinct, goDistinct)
	if distinct < 50 {
		t.Fatalf("%s moved only %d times in 5 ms: coarser than 100 µs", Name, distinct)
	}
}

// Until and Since are the two sides of the same reading.
func TestUntilAndSince(t *testing.T) {
	now := Now()
	later := now.Add(time.Hour)
	if d := Until(later); d <= 59*time.Minute || d > time.Hour {
		t.Fatalf("until an hour from now: %v", d)
	}
	if d := later.Sub(now); d != time.Hour {
		t.Fatalf("sub: %v", d)
	}
	if d := Since(now); d < 0 || d > time.Minute {
		t.Fatalf("since: %v", d)
	}
}
