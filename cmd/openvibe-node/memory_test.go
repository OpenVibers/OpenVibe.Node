package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adapterJSON is the JSON shape of one adapter of the scan.
type adapterJSON struct {
	ID    string     `json:"id"`
	Files []fileJSON `json:"files"`
}

// fileJSON is the JSON shape of one file of the scan.
type fileJSON struct {
	Path  string `json:"path"`
	Found bool   `json:"found"`
}

// itemJSON is the JSON shape of one memory item.
type itemJSON struct {
	Text   string `json:"text"`
	Kind   string `json:"kind"`
	Scope  string `json:"scope"`
	Source string `json:"source"`
}

// memoryFixture is the fixture home of the memory package, used here so the
// test never reads the real home of the machine.
func memoryFixture(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "internal", "memory", "testdata", "claude-code", "home")
}

// TestMemoryScanPrintsEveryAdapter checks the human output of the scan.
func TestMemoryScanPrintsEveryAdapter(t *testing.T) {
	home := memoryFixture(t)
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"scan", "--user-home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the scan failed with %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Claude Code (claude-code)",
		"AGENTS.md (Codex and friends)",
		"Gemini CLI",
		"Cursor",
		"GitHub Copilot",
		"Windsurf",
		"Cline",
		"Roo Code",
		"Kiro",
		"Continue",
		"Aider",
		"Command Code",
		"OpenCode",
		"not there",
		"docs: https://",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output has no %q: %s", want, out)
		}
	}
	if !strings.Contains(out, "Nothing was written and nothing was sent") {
		t.Errorf("the output does not say what it did: %s", out)
	}
}

// TestMemoryScanJSON checks the JSON output of the scan.
func TestMemoryScanJSON(t *testing.T) {
	home := memoryFixture(t)
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"scan", "--user-home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the scan failed with %d: %s", code, stderr.String())
	}
	var res struct {
		Home     string        `json:"home"`
		Adapters []adapterJSON `json:"adapters"`
		Items    []itemJSON    `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("the output is not JSON: %v", err)
	}
	if len(res.Adapters) != len(memoryToolIDs()) {
		t.Errorf("the JSON lists %d adapters, want %d", len(res.Adapters), len(memoryToolIDs()))
	}
	if len(res.Items) == 0 {
		t.Error("the JSON has no items")
	}
	for _, it := range res.Items {
		if it.Source == "" || it.Kind == "" || it.Scope == "" {
			t.Errorf("an item is missing a field: %+v", it)
		}
		if strings.Contains(it.Text, "sk-proj") {
			t.Errorf("the JSON still shows a secret: %q", it.Text)
		}
	}
}

// TestMemoryScanToolsFilter checks that --tools keeps only the adapters asked
// for.
func TestMemoryScanToolsFilter(t *testing.T) {
	home := memoryFixture(t)
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"scan", "--user-home", home, "--tools", "cursor", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the scan failed with %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "\"id\": \"claude-code\"") {
		t.Errorf("the scan read an adapter that was not asked for: %s", stdout.String())
	}
}

// TestMemoryScanUsage checks that a wrong use says how to use it.
func TestMemoryScanUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdMemory([]string{"list"}, &stdout, &stderr); code != 2 {
		t.Errorf("the command returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "memory scan") {
		t.Errorf("the error does not say how to use it: %s", stderr.String())
	}
}

// TestMemoryScanIsReadOnly checks that the scan writes nothing.
func TestMemoryScanIsReadOnly(t *testing.T) {
	home := memoryFixture(t)
	before := fixtureState(t, home)
	var stdout, stderr bytes.Buffer
	if code := cmdMemory([]string{"scan", "--user-home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("the scan failed with %d: %s", code, stderr.String())
	}
	after := fixtureState(t, home)
	if before != after {
		t.Errorf("the scan changed the fixture: %q -> %q", before, after)
	}
}

// fixtureState is a cheap summary of the files under a directory.
func fixtureState(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		out = append(out, path+":"+info.ModTime().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "|")
}
