package node

import (
	"github.com/OpenVibers/OpenVibe.Node/internal/mono"
	"testing"
	"time"
)

// The clock bound is rounded up from nanoseconds (Maticus, 2026-10-27): v0.17.0 truncated a round trip to whole
// microseconds first, so 1,000,001 ns came out as 1 ms.
func TestCeilMSRoundsUpFromNanoseconds(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want int64
	}{
		{0, 0},
		{1, 1},                        // one nanosecond is still a bound
		{999, 1},                      // under a microsecond
		{time.Microsecond, 1},         // exactly one microsecond
		{999_999, 1},                  // a nanosecond under a millisecond
		{time.Millisecond, 1},         // exactly one millisecond
		{1_000_001, 2},                // the reported case
		{1_000_999, 2},                // under a microsecond over (v0.17.0: 1)
		{1_001_000, 2},                // a whole microsecond over
		{2*time.Millisecond - 1, 2},   // a nanosecond under two
		{2 * time.Millisecond, 2},     // exactly two
		{2*time.Millisecond + 1, 3},   // a nanosecond over two
		{25*time.Millisecond - 1, 25}, // a typical round trip less a nanosecond
		{25*time.Millisecond + 1, 26}, // and plus one
		{999_999_999, 1000},           // a nanosecond under MaxClockUncertaintyMS
		{time.Second, 1000},           // exactly MaxClockUncertaintyMS: still admitted
		{time.Second + 1, 1001},       // a nanosecond over: refused
		{time.Hour, 3_600_000},        // no overflow on a long bound
	} {
		if got := ceilMS(c.d); got != c.want {
			t.Errorf("ceilMS(%d ns) = %d, want %d", int64(c.d), got, c.want)
		}
	}
}

// Without a link (local commands) the frame's own ts is the server's clock, and the time left is exact: nothing rounds
// a deadline later. (The link's estimate keeps nanoseconds: link.TestServerNowKeepsNanoseconds.)
func TestTimeLeftIsExact(t *testing.T) {
	n := &Node{}
	left, unc, synced := n.timeLeft(1_000_100, 1_000_000, mono.Now())
	if left != 100*time.Millisecond || unc != 0 || !synced {
		t.Fatalf("left %v unc %d synced %v", left, unc, synced)
	}
}
