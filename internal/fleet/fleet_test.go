package fleet

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseKeepsOnlyWhatAPersonMaySee(t *testing.T) {
	raw := []byte(`[
      {"id":"codex","installed":true,"signed_in":true,"version":"1.2.3","name":"Codex","api_key":"sk-secret",
       "windows":[{"name":"week","used":100,"limit":100,"resets_at":"2026-11-04T00:00:00Z","account":"ada@example.com"}]},
      {"id":"opencode","installed":true,"signed_in":false,"windows":[]},
      {"id":"Bad ID!","installed":true,"signed_in":true},
      {"id":"deepseek","installed":false,"signed_in":false}
    ]`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("agents: %+v", got)
	}
	if got[0].Agent != "codex" || !got[0].Installed || !got[0].SignedIn {
		t.Fatalf("codex: %+v", got[0])
	}
	if len(got[0].Windows) != 1 || got[0].Windows[0].Name != "week" || got[0].Windows[0].Used == nil || *got[0].Windows[0].Used != 100 ||
		got[0].Windows[0].ResetsAt != "2026-11-04T00:00:00Z" {
		t.Fatalf("window: %+v", got[0].Windows)
	}
	if got[1].Agent != "opencode" || got[1].SignedIn {
		t.Fatalf("opencode: %+v", got[1])
	}
	if got[2].Agent != "deepseek" || got[2].Installed {
		t.Fatalf("deepseek: %+v", got[2])
	}
}

func TestParseIsBounded(t *testing.T) {
	var rows []byte
	rows = append(rows, '[')
	for i := 0; i < 40; i++ {
		if i > 0 {
			rows = append(rows, ',')
		}
		rows = append(rows, []byte(`{"id":"agent-`+"0123456789"[i%10:i%10+1]+`","windows":[{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"},{"name":"w"}]}`)...)
	}
	rows = append(rows, ']')
	got, err := Parse(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxAgents {
		t.Fatalf("agents: %d", len(got))
	}
	for _, a := range got {
		if len(a.Windows) > MaxWindows {
			t.Fatalf("%s windows: %d", a.Agent, len(a.Windows))
		}
	}
}

func TestProberReadsTheRunnerAndKeepsItsLastAnswer(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '[{\"id\":\"codex\",\"installed\":true,\"signed_in\":true,\"windows\":[{\"name\":\"week\",\"used\":10,\"limit\":100,\"resets_at\":\"2026-11-04T00:00:00Z\"}]}]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Command: []string{"/bin/sh", script}, Log: quiet()})
	if got := p.Latest(); got != nil {
		t.Fatalf("nothing is reported before the first probe: %+v", got)
	}
	p.probe(context.Background())
	got := p.Latest()
	if len(got) != 1 || got[0].Agent != "codex" || len(got[0].Windows) != 1 {
		t.Fatalf("probe: %+v", got)
	}
	if p.At().IsZero() {
		t.Fatal("the reading has a time")
	}

	// A probe that fails keeps the last reading rather than reporting an empty machine.
	broken := New(Options{Command: []string{"/bin/sh", "-c", "exit 3"}, Log: quiet()})
	if err := os.WriteFile(filepath.Join(dir, "bad.sh"), []byte("#!/bin/sh\necho 'not json'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.opt.Command = []string{"/bin/sh", filepath.Join(dir, "bad.sh")}
	p.probe(context.Background())
	if got := p.Latest(); len(got) != 1 {
		t.Fatalf("the last reading stands: %+v", got)
	}
	broken.probe(context.Background())
	if got := broken.Latest(); got != nil {
		t.Fatalf("a machine that never answered reports nothing: %+v", got)
	}
}

func TestRunProbesAgainOnItsInterval(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := filepath.Join(dir, "probe.sh")
	src := "#!/bin/sh\necho x >> " + count + "\necho '[]'\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Command: []string{"/bin/sh", script}, Interval: 20 * time.Millisecond, Log: quiet()})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	data, _ := os.ReadFile(count)
	if len(data) < 2 {
		t.Fatalf("the prober ran once: %q", data)
	}
}
