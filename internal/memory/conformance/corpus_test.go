// Package conformance runs the shared redaction corpus (Memory M2a) against the Node's Go scrubber. It lives in its
// own package because it reads the network to compare the copy with the SDK tag it names; internal/memory itself is
// deliberately local-only (memory_test.go fails on any net import).
package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/memory"
)

type corpusCase struct {
	Name           string   `json:"name"`
	In             string   `json:"in"`
	MustNotContain []string `json:"mustNotContain"`
	MustContain    []string `json:"mustContain"`
}

type corpusFile struct {
	From  string       `json:"from"`
	Cases []corpusCase `json:"cases"`
}

const corpusPath = "../testdata/redact-corpus.json"

func readCorpus(t *testing.T) corpusFile {
	t.Helper()
	data, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	var c corpusFile
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.From != "openvibe-sdk v0.75.0" {
		t.Fatalf(`the corpus says "from": %q, want "openvibe-sdk v0.75.0"`, c.From)
	}
	if len(c.Cases) < 50 {
		t.Fatalf("the corpus has %d cases, want at least 50", len(c.Cases))
	}
	return c
}

// TestRedactCorpus runs every shared case against the Go scrubber: the secret goes, the words around it stay, the
// count is right, and harmless text is untouched.
func TestRedactCorpus(t *testing.T) {
	for _, c := range readCorpus(t).Cases {
		if c.Name == "" || c.In == "" {
			t.Fatalf("a case has no name or input: %+v", c)
		}
		out, n := memory.Redact(c.In)
		for _, secret := range c.MustNotContain {
			if strings.Contains(out, secret) {
				t.Errorf("%s: %q is still there: %q", c.Name, secret, out)
			}
		}
		for _, keep := range c.MustContain {
			if !strings.Contains(out, keep) {
				t.Errorf("%s: the text lost %q: %q", c.Name, keep, out)
			}
		}
		if len(c.MustNotContain) == 0 {
			if n != 0 {
				t.Errorf("%s: harmless text was counted as a secret", c.Name)
			}
			if out != c.In {
				t.Errorf("%s: harmless text was changed: %q", c.Name, out)
			}
			continue
		}
		if n == 0 {
			t.Errorf("%s: nothing was redacted", c.Name)
		}
		again, more := memory.Redact(out)
		if again != out || more != 0 {
			t.Errorf("%s: a second pass changed or counted something: %q (%d)", c.Name, again, more)
		}
	}
}

// TestRedactCorpusMatchesSDK compares the copy with the file at the SDK tag it names. A machine with no network
// skips it; a mismatch is always a failure.
func TestRedactCorpusMatchesSDK(t *testing.T) {
	local := readCorpus(t)
	version := strings.TrimPrefix(local.From, "openvibe-sdk ")
	url := fmt.Sprintf("https://raw.githubusercontent.com/OpenVibers/OpenVibe.SDK/%s/test/redact-corpus.json", version)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		t.Skipf("no network to compare the corpus with %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Skipf("the SDK tag did not serve the corpus (%s: %s)", url, res.Status)
	}
	var remote corpusFile
	if err := json.NewDecoder(res.Body).Decode(&remote); err != nil {
		t.Fatalf("the corpus at %s is not JSON: %v", url, err)
	}
	if !reflect.DeepEqual(remote.Cases, local.Cases) {
		t.Fatalf("the local corpus differs from %s: copy it again and update \"from\"", url)
	}
}
