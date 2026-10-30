package memorystore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request the fake store received.
type recorded struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

// fakeStore is a store that answers what the test tells it to.
type fakeStore struct {
	mu       sync.Mutex
	requests []recorded
	// answers is the list of answers, one per request; the last one repeats.
	answers []func(w http.ResponseWriter, r *http.Request)
}

// newFakeStore starts a fake store.
func newFakeStore(t *testing.T, answers ...func(w http.ResponseWriter, r *http.Request)) (*fakeStore, *httptest.Server) {
	t.Helper()
	fs := &fakeStore{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		fs.mu.Lock()
		fs.requests = append(fs.requests, recorded{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
		var answer func(w http.ResponseWriter, r *http.Request)
		if len(fs.answers) == 0 {
			answer = okAnswer
		} else if len(fs.requests) <= len(fs.answers) {
			answer = fs.answers[len(fs.requests)-1]
		} else {
			answer = fs.answers[len(fs.answers)-1]
		}
		fs.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return fs, srv
}

// readBody reads a request body.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	raw, _ := io.ReadAll(r.Body)
	return raw
}

// okAnswer answers with an empty import.
func okAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"items":[],"count":0,"created":0,"deduped":0}`))
}

// seen gives the requests the fake store received.
func (fs *fakeStore) seen() []recorded {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]recorded(nil), fs.requests...)
}

// TestUploadBatchesInTwoHundred checks that the items go in batches of 200.
func TestUploadBatchesInTwoHundred(t *testing.T) {
	fs, srv := newFakeStore(t)
	client, err := New(Options{BaseURL: srv.URL, Token: "person-token", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]Item, 0, 401)
	for i := 0; i < 401; i++ {
		items = append(items, Item{Scope: "user", Kind: "fact", Text: "item", Source: "claude-code"})
	}
	res, err := client.Upload(context.Background(), items)
	if err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	reqs := fs.seen()
	if len(reqs) != 3 {
		t.Fatalf("the store got %d requests, want 3", len(reqs))
	}
	for i, want := range []int{200, 200, 1} {
		var body struct {
			Items []Item `json:"items"`
		}
		if err := json.Unmarshal(reqs[i].Body, &body); err != nil {
			t.Fatalf("the body is not JSON: %v", err)
		}
		if len(body.Items) != want {
			t.Errorf("request %d carried %d items, want %d", i, len(body.Items), want)
		}
	}
	if res.Created != 0 {
		t.Errorf("the store said %d items were created, want 0", res.Created)
	}
}

// TestUploadCountsCreatedAndDeduped checks the counts of the store's answer.
func TestUploadCountsCreatedAndDeduped(t *testing.T) {
	_, srv := newFakeStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"count":5,"created":3,"deduped":2}`))
	})
	client, err := New(Options{BaseURL: srv.URL, Token: "t", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}})
	if err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	if res.Created != 3 || res.Deduplicated != 2 {
		t.Errorf("the counts are %d created and %d deduped, want 3 and 2", res.Created, res.Deduplicated)
	}
}

// TestUploadRetriesOnABusyStore checks that a 429 and a 500 are tried again.
func TestUploadRetriesOnABusyStore(t *testing.T) {
	fs, srv := newFakeStore(t,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		okAnswer,
	)
	client, err := New(Options{BaseURL: srv.URL, Token: "t", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}}); err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	if len(fs.seen()) != 3 {
		t.Errorf("the store got %d requests, want 3 (the two refusals and the answer)", len(fs.seen()))
	}
}

// TestUploadGivesUpOnABrokenStore checks that a store that stays broken ends
// the upload with an error.
func TestUploadGivesUpOnABrokenStore(t *testing.T) {
	_, srv := newFakeStore(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	client, err := New(Options{BaseURL: srv.URL, Token: "t", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}})
	if err == nil {
		t.Fatal("the upload said it worked, but the store was broken")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("the error is not an APIError: %v", err)
	}
	if !apiErr.Retryable() {
		t.Errorf("the error is not retryable: %v", err)
	}
}

// TestUploadDropsTheItemTheStoreRefuses checks that one refused item does not
// stop the rest.
func TestUploadDropsTheItemTheStoreRefuses(t *testing.T) {
	refuse := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"code":"memory.secret_only","detail":"items[1] is only a secret and is refused"}`))
	}
	fs, srv := newFakeStore(t, refuse, okAnswer)
	client, err := New(Options{BaseURL: srv.URL, Token: "t", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{Scope: "user", Kind: "fact", Text: "one", Source: "claude-code"},
		{Scope: "user", Kind: "fact", Text: "two", Source: "claude-code"},
	}
	res, err := client.Upload(context.Background(), items)
	if err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	if res.Refused != 1 {
		t.Errorf("the client counted %d refused items, want 1", res.Refused)
	}
	reqs := fs.seen()
	if len(reqs) != 2 {
		t.Fatalf("the store got %d requests, want 2", len(reqs))
	}
	var body struct {
		Items []Item `json:"items"`
	}
	if err := json.Unmarshal(reqs[1].Body, &body); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Text != "one" {
		t.Errorf("the second request carried %+v, want only the first item", body.Items)
	}
}

// TestUploadSendsTheTokenAsABearer checks how the client authenticates.
func TestUploadSendsTheTokenAsABearer(t *testing.T) {
	fs, srv := newFakeStore(t)
	client, err := New(Options{BaseURL: srv.URL, Token: "the-person-token", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}}); err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	reqs := fs.seen()
	if len(reqs) != 1 {
		t.Fatalf("the store got %d requests, want 1", len(reqs))
	}
	if reqs[0].Auth != "Bearer the-person-token" {
		t.Errorf("the request carried %q, want the person's token", reqs[0].Auth)
	}
	if reqs[0].Path != "/api/v1/memory/items:batch" {
		t.Errorf("the request went to %q, want the batch path", reqs[0].Path)
	}
}

// TestUploadWithoutATokenSaysSo checks that a client with no token does not
// call the store at all.
func TestUploadWithoutATokenSaysSo(t *testing.T) {
	fs, srv := newFakeStore(t)
	client, err := New(Options{BaseURL: srv.URL, Token: "  "})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}})
	if err == nil {
		t.Fatal("the upload worked without a token")
	}
	if !strings.Contains(err.Error(), "OPENVIBE_TOKEN") {
		t.Errorf("the error does not say which variable is missing: %v", err)
	}
	if len(fs.seen()) != 0 {
		t.Errorf("the store got %d requests, want none", len(fs.seen()))
	}
}

// TestTheTokenNeverAppearsInAnError checks that the token is not in the
// message of a refusal.
func TestTheTokenNeverAppearsInAnError(t *testing.T) {
	_, srv := newFakeStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":401,"code":"token.invalid","detail":"the token the-person-token is not valid"}`))
	})
	client, err := New(Options{BaseURL: srv.URL, Token: "the-person-token", Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Upload(context.Background(), []Item{{Scope: "user", Kind: "fact", Text: "x", Source: "claude-code"}})
	if err == nil {
		t.Fatal("the upload worked with a refused token")
	}
	if strings.Contains(err.Error(), "the-person-token") {
		t.Errorf("the error holds the token: %v", err)
	}
}

// TestListGivesTheItems checks that the list is asked for and read.
func TestListGivesTheItems(t *testing.T) {
	_, srv := newFakeStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"mem_1","scope":"user","kind":"fact","text":"hello","source":"claude-code","status":"active"}],"count":1}`))
	})
	client, err := New(Options{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.List(context.Background(), "", "")
	if err != nil {
		t.Fatalf("the list failed: %v", err)
	}
	if res.Count != 1 || len(res.Items) != 1 || res.Items[0].Text != "hello" {
		t.Errorf("the list is %+v, want one item with the text hello", res)
	}
}

// TestNewRefusesAPlainAddress checks that the client does not talk to the
// store over plain http.
func TestNewRefusesAPlainAddress(t *testing.T) {
	if _, err := New(Options{BaseURL: "http://ai.example.com", Token: "t"}); err == nil {
		t.Error("the client accepted a plain http store")
	}
	if _, err := New(Options{BaseURL: "https://ai.example.com", Token: "t"}); err != nil {
		t.Errorf("the client refused an https store: %v", err)
	}
}

// asAPIError is errors.As without the import, kept short for the tests.
func asAPIError(err error, target **APIError) bool {
	if e, ok := err.(*APIError); ok {
		*target = e
		return true
	}
	return false
}
