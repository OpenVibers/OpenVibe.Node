package fleet

import (
	"context"
	"fmt"
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

// The probe runs a command; the tests run the test binary itself with a mode in the environment, the way the Go
// standard library does it, so the test says the same thing on every platform (a shell script is no Windows).
const helperEnv = "CODES_FLEET_HELPER"
const helperCountEnv = "CODES_FLEET_HELPER_COUNT"

// TestFleetHelperProcess is the probe the tests below run: it is not a test of its own.
func TestFleetHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "ok":
		fmt.Fprint(os.Stdout, `[{"id":"codex","installed":true,"signed_in":true,"windows":[{"name":"week","used":10,"limit":100,"resets_at":"2026-11-04T00:00:00Z"}]}]`)
	case "empty":
		fmt.Fprint(os.Stdout, `[]`)
	case "bad":
		fmt.Fprint(os.Stdout, "not json")
	case "count":
		if path := os.Getenv(helperCountEnv); path != "" {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(f, "x")
				f.Close()
			}
		}
		fmt.Fprint(os.Stdout, `[]`)
	}
	os.Exit(0)
}

// The probe command: this test binary, asked for the helper.
func helperCommand() []string { return []string{os.Args[0], "-test.run=TestFleetHelperProcess"} }

// withHelper sets the mode the helper answers with, for the time of one test.
func withHelper(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(helperEnv, mode)
}

func TestProberReadsTheRunnerAndKeepsItsLastAnswer(t *testing.T) {
	withHelper(t, "ok")
	p := New(Options{Command: helperCommand(), Log: quiet()})
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

	// A probe that answers something else keeps the last reading rather than reporting an empty machine.
	withHelper(t, "bad")
	p.probe(context.Background())
	if got := p.Latest(); len(got) != 1 {
		t.Fatalf("the last reading stands: %+v", got)
	}

	// A machine that never answered reports nothing.
	withHelper(t, "bad")
	broken := New(Options{Command: helperCommand(), Log: quiet()})
	broken.probe(context.Background())
	if got := broken.Latest(); got != nil {
		t.Fatalf("a machine that never answered reports nothing: %+v", got)
	}
}

func TestRunProbesAgainOnItsInterval(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv(helperCountEnv, count)
	withHelper(t, "count")
	p := New(Options{Command: helperCommand(), Interval: 20 * time.Millisecond, Log: quiet()})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	data, _ := os.ReadFile(count)
	if len(data) < 2 {
		t.Fatalf("the prober ran once: %q", data)
	}
}
