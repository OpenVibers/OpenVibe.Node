package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// memoryStore is a fake memory store that records what it is sent.
type memoryStore struct {
	mu       sync.Mutex
	requests [][]byte
	paths    []string
	answer   string
}

// newMemoryStore starts a fake store.
func newMemoryStore(t *testing.T) (*memoryStore, *httptest.Server) {
	t.Helper()
	ms := &memoryStore{answer: `{"items":[],"count":0,"created":0,"deduped":0}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		ms.mu.Lock()
		ms.requests = append(ms.requests, raw)
		ms.paths = append(ms.paths, r.URL.Path)
		answer := ms.answer
		ms.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return ms, srv
}

// sent gives the bodies the store received.
func (ms *memoryStore) sent() [][]byte {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return append([][]byte(nil), ms.requests...)
}

// uploadFixture is the fixture home of the memory package and its project.
func uploadFixture(t *testing.T) (string, string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..", "internal", "memory", "testdata", "claude-code")
	return filepath.Join(root, "home"), filepath.Join(root, "project")
}

// TestMemoryUploadDryRunSendsNothing checks that a dry run shows the summary
// and sends nothing.
func TestMemoryUploadDryRunSendsNothing(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--dry-run", "--endpoint", srv.URL},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	if len(ms.sent()) != 0 {
		t.Errorf("the store got %d requests, want none", len(ms.sent()))
	}
	out := stdout.String()
	for _, want := range []string{
		"Claude Code (claude-code)",
		"items to send",
		"Scopes:",
		"project:project",
		"Nothing was sent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output has no %q: %s", want, out)
		}
	}
}

// TestMemoryUploadAsksBeforeSending checks that nothing is sent until the
// person says yes.
func TestMemoryUploadAsksBeforeSending(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--endpoint", srv.URL},
		strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	if len(ms.sent()) != 0 {
		t.Errorf("the store got %d requests after a no, want none", len(ms.sent()))
	}
	if !strings.Contains(stdout.String(), "Nothing was sent") {
		t.Errorf("the output does not say that nothing was sent: %s", stdout.String())
	}
}

// TestMemoryUploadSendsWhenThePersonSaysYes checks that a yes sends the items.
func TestMemoryUploadSendsWhenThePersonSaysYes(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--endpoint", srv.URL},
		strings.NewReader("yes\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	if len(ms.sent()) != 1 {
		t.Fatalf("the store got %d requests, want 1", len(ms.sent()))
	}
	if !strings.Contains(stdout.String(), "Sent:") {
		t.Errorf("the output does not say what was sent: %s", stdout.String())
	}
}

// TestMemoryUploadYesSkipsTheQuestion checks that --yes sends without asking.
func TestMemoryUploadYesSkipsTheQuestion(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--yes", "--endpoint", srv.URL},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	if len(ms.sent()) != 1 {
		t.Errorf("the store got %d requests, want 1", len(ms.sent()))
	}
	if strings.Contains(stdout.String(), "[y/N]") {
		t.Errorf("the output asked a question it did not need to ask: %s", stdout.String())
	}
}

// TestMemoryUploadSendsNothingOfTheMachine checks the bodies that leave: no
// absolute path, no hostname, no name of the person.
func TestMemoryUploadSendsNothingOfTheMachine(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--yes", "--endpoint", srv.URL},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}
	for _, body := range ms.sent() {
		text := string(body)
		for _, secret := range []string{home, project, hostname, userName, os.Getenv("HOME")} {
			if secret == "" {
				continue
			}
			if strings.Contains(text, secret) {
				t.Errorf("the request holds %q, which is a fact about the machine: %s", secret, text)
			}
		}
		if strings.Contains(text, "/home/") {
			t.Errorf("the request holds an absolute path: %s", text)
		}
	}
}

// TestMemoryUploadSendsTheItemsOfTheScan checks what the request holds: the
// text, the kind, the scope, the adapter and the place the item was written.
func TestMemoryUploadSendsTheItemsOfTheScan(t *testing.T) {
	home, project := uploadFixture(t)
	ms, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--project", project, "--yes", "--endpoint", srv.URL},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the upload failed with %d: %s", code, stderr.String())
	}
	bodies := ms.sent()
	if len(bodies) != 1 {
		t.Fatalf("the store got %d requests, want 1", len(bodies))
	}
	var sent struct {
		Items []struct {
			Scope     string `json:"scope"`
			Kind      string `json:"kind"`
			Text      string `json:"text"`
			Source    string `json:"source"`
			SourceRef struct {
				Path    string `json:"path"`
				Heading string `json:"heading"`
			} `json:"source_ref"`
		} `json:"items"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("the request is not JSON: %v", err)
	}
	if len(sent.Items) == 0 {
		t.Fatal("the request holds no items")
	}
	scopes := map[string]bool{}
	for _, it := range sent.Items {
		scopes[it.Scope] = true
		if it.Text == "" {
			t.Error("an item has no text")
		}
		if it.Source != "claude-code" {
			t.Errorf("an item has the source %q, want the adapter id", it.Source)
		}
		if it.Kind == "" {
			t.Error("an item has no kind")
		}
		if strings.HasPrefix(it.SourceRef.Path, "/") {
			t.Errorf("an item has the absolute path %q", it.SourceRef.Path)
		}
	}
	if !scopes["user"] {
		t.Error("the request holds no item of the person")
	}
	if !scopes["project:project"] {
		t.Errorf("the request holds no item of the project: %v", scopes)
	}
}

// TestMemoryUploadNeedsAToken checks that the command says which variable is
// missing.
func TestMemoryUploadNeedsAToken(t *testing.T) {
	home, _ := uploadFixture(t)
	_, srv := newMemoryStore(t)
	t.Setenv("OPENVIBE_TOKEN", "")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"upload", "--user-home", home, "--yes", "--endpoint", srv.URL},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("the upload returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "OPENVIBE_TOKEN") {
		t.Errorf("the error does not name the variable: %s", stderr.String())
	}
}

// TestMemoryStatusCountsWithoutTheText checks what the status shows: the
// counts per source and per scope, and no text unless it is asked for.
func TestMemoryStatusCountsWithoutTheText(t *testing.T) {
	ms, srv := newMemoryStore(t)
	ms.answer = `{"items":[{"id":"mem_1","scope":"user","kind":"fact","text":"the secret of the day","source":"claude-code","status":"active"},
		{"id":"mem_2","scope":"project:my-project","kind":"fact","text":"another one","source":"cursor","status":"active"}],"count":2}`
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"status", "--endpoint", srv.URL}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the status failed with %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"2 items", "claude-code: 1", "cursor: 1", "user: 1", "project:my-project: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output has no %q: %s", want, out)
		}
	}
	if strings.Contains(out, "the secret of the day") {
		t.Errorf("the output shows the text without --show: %s", out)
	}
}

// TestMemoryStatusShowsTheTextWithShow checks that --show prints the items.
func TestMemoryStatusShowsTheTextWithShow(t *testing.T) {
	ms, srv := newMemoryStore(t)
	ms.answer = `{"items":[{"id":"mem_1","scope":"user","kind":"fact","text":"the secret of the day","source":"claude-code","status":"active"}],"count":1}`
	t.Setenv("OPENVIBE_TOKEN", "the-person-token")
	var stdout, stderr bytes.Buffer
	code := cmdMemory([]string{"status", "--show", "--endpoint", srv.URL}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the status failed with %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "the secret of the day") {
		t.Errorf("the output does not show the item: %s", stdout.String())
	}
}

// TestMemoryUploadUsage checks that a wrong use says how to use it.
func TestMemoryUploadUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdMemory([]string{"upload", "--nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("the command returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "memory upload") {
		t.Errorf("the error does not say how to use it: %s", stderr.String())
	}
}
