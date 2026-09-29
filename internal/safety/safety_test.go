package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

func f(v float64) *float64 { return &v }

func TestLatchPersistsAcrossRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "latch.json")
	l, err := OpenLatch(p)
	if err != nil || l.State().Stopped() {
		t.Fatalf("fresh latch: %v %+v", err, l.State())
	}
	var seen []LatchState
	l.OnChange(func(s LatchState) { seen = append(seen, s) })
	l.SetRemote("owner pressed stop")
	l2, _ := OpenLatch(p)
	if !l2.State().Remote || l2.State().RemoteReason != "owner pressed stop" {
		t.Fatalf("not persisted: %+v", l2.State())
	}
	l.SetLocal()
	l.ClearRemote()
	if st := l.State(); st.Remote || !st.Local {
		t.Fatalf("estop_clear must not clear the local stop: %+v", st)
	}
	l.Resume()
	if l.State().Stopped() {
		t.Fatal("resume did not clear")
	}
	if len(seen) != 4 {
		t.Fatalf("callbacks %d", len(seen))
	}
}

func TestCorruptLatchFailsSafe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "latch.json")
	os.WriteFile(p, []byte("{nope"), 0o600)
	l, err := OpenLatch(p)
	if err == nil || !l.State().Local {
		t.Fatalf("corrupt latch must hold the local stop: %v %+v", err, l.State())
	}
}

func TestReloadSeesCLIWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "latch.json")
	l, _ := OpenLatch(p)
	if ch, _ := l.Reload(); ch {
		t.Fatal("spurious change")
	}
	if err := WriteLocalFile(p, true); err != nil {
		t.Fatal(err)
	}
	if ch, _ := l.Reload(); !ch || !l.State().Local {
		t.Fatalf("reload missed the stop: %+v", l.State())
	}
	WriteLocalFile(p, false)
	if ch, _ := l.Reload(); !ch || l.State().Stopped() {
		t.Fatalf("reload missed the resume: %+v", l.State())
	}
}

func TestMerge(t *testing.T) {
	l := Merge(protocol.Limits{MaxSpeed: f(0.8), MaxTurn: f(0.5), MaxCommandMS: 600}, f(0.3), nil, 400)
	if l.MaxSpeed != 0.3 || l.MaxTurn != 0.5 || l.MaxCommandMS != 400 {
		t.Fatalf("%+v", l)
	}
	if d := DefaultLimits(); d.MaxSpeed != 1 || d.MaxCommandMS != protocol.DefaultMaxCommandMS {
		t.Fatalf("%+v", d)
	}
}

func TestDeadline(t *testing.T) {
	l := Limits{MaxCommandMS: 500}
	for in, want := range map[int]int{0: 300, -5: 300, 100: 100, 500: 500, 9000: 500} {
		if got := l.Deadline(in); got != want {
			t.Errorf("Deadline(%d)=%d want %d", in, got, want)
		}
	}
}

func TestClamp(t *testing.T) {
	l := Limits{MaxSpeed: 0.5, MaxTurn: 0.25, MaxCommandMS: 1000}
	out, err := l.Clamp("drive", json.RawMessage(`{"throttle":1,"steer":-1,"radius":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]float64
	json.Unmarshal(out, &v)
	if v["throttle"] != 0.5 || v["steer"] != -0.25 || v["radius"] != 3 {
		t.Fatalf("%s", out)
	}
	out, _ = l.Clamp("drive", json.RawMessage(`{"x":-9,"y":0.1,"rotation":0.9}`))
	json.Unmarshal(out, &v)
	if v["x"] != -0.5 || v["y"] != 0.1 || v["rotation"] != 0.25 {
		t.Fatalf("%s", out)
	}
	out, _ = l.Clamp("ptz", json.RawMessage(`{"pan":4,"tilt":-0.5}`))
	json.Unmarshal(out, &v)
	if v["pan"] != 1 || v["tilt"] != -0.5 {
		t.Fatalf("%s", out)
	}
	for kind, bad := range map[string]string{
		"drive":    `{"throttle":"fast"}`,
		"ptz":      `{"pan":null}`,
		"actuator": `{"value":1}`,
		"say":      `{}`,
	} {
		if _, err := l.Clamp(kind, json.RawMessage(bad)); err == nil {
			t.Errorf("%s %s accepted", kind, bad)
		}
	}
	if _, err := l.Clamp("drive", json.RawMessage(`[1]`)); err == nil {
		t.Error("array accepted")
	}
	if _, err := l.Clamp("halt", nil); err != nil {
		t.Error(err)
	}
}

func TestDedup(t *testing.T) {
	d := NewDedup(2, time.Minute)
	e, first := d.Begin("a")
	if !first {
		t.Fatal("first")
	}
	e2, first2 := d.Begin("a")
	if first2 || e2 != e {
		t.Fatal("duplicate treated as new")
	}
	d.Complete(e, protocol.Ack{ID: "a"})
	d.Complete(e, protocol.Nack{ID: "a"})
	<-e2.Done()
	if _, ok := e2.Result().(protocol.Ack); !ok {
		t.Fatal("first result not kept")
	}
	d.Begin("b")
	d.Begin("c")
	d.Begin("d")
	if _, first := d.Begin("a"); !first {
		t.Fatal("capacity not enforced")
	}
	now := time.Now()
	d.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, first := d.Begin("d"); !first {
		t.Fatal("ttl not enforced")
	}
}
