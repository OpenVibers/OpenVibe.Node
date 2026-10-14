package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestProfileVerify: `profile verify` checks a community profile against Bot's signature (the fixture Bot's own code
// signed) and names its plugin; another pinned key, an unsigned profile or a built-in id fail.
func TestProfileVerify(t *testing.T) {
	b, err := os.ReadFile("../../internal/profilesig/testdata/ballbot.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Served map[string]json.RawMessage `json:"served"`
		Key    json.RawMessage            `json:"key"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	signed := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/profiles/signing-key":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": f.Key})
		case "/api/v1/profiles/community.ballbot":
			doc := map[string]any{"profile": f.Served["profile"]}
			if signed {
				doc["signature"] = f.Served["signature"]
			}
			_ = json.NewEncoder(w).Encode(doc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := t.TempDir()

	out, errOut, code := cli(t, "profile", "verify", "community.ballbot", "--server", srv.URL, "--home", home)
	if code != 0 || !strings.Contains(out, "as reviewed") || !strings.Contains(out, `"ballbot"`) {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	if _, errOut, code := cli(t, "profile", "verify", "community.ballbot", "--server", srv.URL, "--key-id", "ffffffffffffffff", "--home", home); code == 0 || !strings.Contains(errOut, "do not install") {
		t.Fatalf("another pinned key: code %d %s", code, errOut)
	}
	if _, errOut, code := cli(t, "profile", "verify", "adeept.adr036", "--server", srv.URL, "--home", home); code == 0 || !strings.Contains(errOut, "not a community profile") {
		t.Fatalf("a built-in profile: code %d %s", code, errOut)
	}
	if _, _, code := cli(t, "profile", "check", "community.ballbot", "--home", home); code != 2 {
		t.Fatalf("a wrong subcommand: code %d", code)
	}
	signed = false
	if _, errOut, code := cli(t, "profile", "verify", "community.ballbot", "--server", srv.URL, "--home", home); code == 0 || !strings.Contains(errOut, "no signature") {
		t.Fatalf("unsigned: code %d %s", code, errOut)
	}
}
