// Package memorystore is the client of the person's own memory store: the
// OpenVibe.AI module behind /api/v1/memory.
//
// The store is scoped to one person: the person's own Network access token
// (the audience openvibe.network) is the only thing that opens it, and every
// query the server runs is scoped by the owner. This client sends the person's
// token as a Bearer and nothing else about the machine: the text of an item,
// its kind, its scope, the adapter it came from and where it was written.
//
// The token is a secret: it is never printed, never written to an error
// message and never put in a URL.
package memorystore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is where the store lives.
const DefaultBaseURL = "https://ai.openvibe.services"

// BatchSize is how many items one request may carry. The server takes 200.
const BatchSize = 200

// MaxAttempts is how many times a request is tried when the server is busy or
// broken.
const MaxAttempts = 4

// MinTokenLength is the length a token must have before it is taken out of a
// message. A shorter one would match inside every other word.
const MinTokenLength = 8

// DefaultTokenEnv is the environment variable the person's access token is
// read from. It is the same token the OpenVibe.Codes CLI uses.
const DefaultTokenEnv = "OPENVIBE_TOKEN"

// SourceRef says where an item was written: the path of the file relative to
// the scanned root, and the heading it was under. It never holds an absolute
// path.
type SourceRef struct {
	Path    string `json:"path,omitempty"`
	Heading string `json:"heading,omitempty"`
}

// Item is one memory item on the wire.
type Item struct {
	// Scope is `user` or `project:<slug>`. Never an absolute path.
	Scope string `json:"scope"`
	// Kind is one of preference, fact, project_rule, procedure, decision, lesson.
	Kind string `json:"kind"`
	// Text is the item itself, already redacted.
	Text string `json:"text"`
	// Source is the id of the adapter that found it.
	Source string `json:"source"`
	// SourceRef says where it was written.
	SourceRef *SourceRef `json:"source_ref,omitempty"`
	// Confidence is how sure the item is, 0 to 1.
	Confidence float64 `json:"confidence,omitempty"`
	// NoLooks says the item was written by an AI, so it is a suggestion and
	// may not read as a fact about the person.
	NoLooks bool `json:"no_looks,omitempty"`
}

// BatchResult is what the store answered about an import.
type BatchResult struct {
	// Created is how many items were new.
	Created int
	// Deduplicated is how many items were already there.
	Deduplicated int
	// Refused is how many items the store would not take.
	Refused int
}

// ListItem is one item of the person's own list.
type ListItem struct {
	ID     string     `json:"id"`
	Scope  string     `json:"scope"`
	Kind   string     `json:"kind"`
	Text   string     `json:"text"`
	Source string     `json:"source"`
	Status string     `json:"status"`
	Ref    *SourceRef `json:"source_ref,omitempty"`
}

// ListResult is the person's own items.
type ListResult struct {
	Items []ListItem
	Count int
}

// APIError is a refusal of the store.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is the problem code, e.g. `input.invalid`.
	Code string
	// Detail says what the store refused.
	Detail string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("the memory store refused this with HTTP %d", e.Status)
	}
	return fmt.Sprintf("the memory store refused this with HTTP %d (%s): %s", e.Status, e.Code, e.Detail)
}

// Retryable says whether trying again may help: the server is busy or broken.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// ErrNoToken says there is no person token to open the store with.
var ErrNoToken = errors.New("no person token: set OPENVIBE_TOKEN (or the variable --token-env names) to your OpenVibe access token")

// Client is the store client of one person.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// Backoff waits before a retry. It is a field so the tests need not wait.
	backoff func(attempt int) time.Duration
}

// Options configure a Client.
type Options struct {
	// BaseURL is the origin of the store. Empty means DefaultBaseURL.
	BaseURL string
	// Token is the person's access token.
	Token string
	// HTTP is the client to use. Empty means a client with a 30 s timeout.
	HTTP *http.Client
	// Backoff waits before a retry. Empty means an exponential wait with jitter.
	Backoff func(attempt int) time.Duration
}

// New makes a client of the store.
func New(opts Options) (*Client, error) {
	base := strings.TrimSuffix(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("the memory store address %q is not an URL", opts.BaseURL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("refusing to talk to the memory store over %s", u.Scheme)
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{
		baseURL: base,
		token:   strings.TrimSpace(opts.Token),
		http:    httpClient,
		backoff: opts.Backoff,
	}
	if c.backoff == nil {
		c.backoff = defaultBackoff
	}
	return c, nil
}

// defaultBackoff waits a little longer every time, with jitter, so a busy
// store is not hammered by every machine at once.
func defaultBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond * time.Duration(1<<uint(attempt-1))
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	return base/2 + time.Duration(rand.Int63n(int64(base/2)+1))
}

// isLoopback tells whether a host is the machine itself.
func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// Upload sends the items to the store, in batches of 200.
//
// An item the store refuses is counted and the rest of the batch is sent
// again; a store that is busy or broken is tried again with a wait. The text
// of an item never appears in an error.
func (c *Client) Upload(ctx context.Context, items []Item) (BatchResult, error) {
	var out BatchResult
	for start := 0; start < len(items); start += BatchSize {
		end := start + BatchSize
		if end > len(items) {
			end = len(items)
		}
		res, refused, err := c.uploadChunk(ctx, items[start:end])
		out.Created += res.Created
		out.Deduplicated += res.Deduplicated
		out.Refused += refused
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// uploadChunk sends one batch and returns what the store made of it. A refused
// item is dropped and the batch is sent again, at most a few times.
func (c *Client) uploadChunk(ctx context.Context, chunk []Item) (BatchResult, int, error) {
	refused := 0
	for attempt := 0; attempt < 10 && len(chunk) > 0; attempt++ {
		res, err := c.postBatch(ctx, chunk)
		if err == nil {
			res.Refused = refused
			return res, refused, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			index, ok := refusedIndex(apiErr)
			if !ok {
				return BatchResult{}, refused, err
			}
			// The store named the item it would not take: drop it and send the rest.
			refused++
			chunk = append(chunk[:index:index], chunk[index+1:]...)
			continue
		}
		if attempt == MaxAttempts-1 {
			return BatchResult{}, refused, err
		}
		select {
		case <-ctx.Done():
			return BatchResult{}, refused, ctx.Err()
		case <-time.After(c.backoff(attempt + 1)):
		}
	}
	return BatchResult{}, refused, errors.New("the memory store refused too many items")
}

// refusedIndex finds the item the store named in its refusal.
func refusedIndex(err *APIError) (int, bool) {
	if err == nil {
		return 0, false
	}
	const marker = "items["
	at := strings.Index(err.Detail, marker)
	if at < 0 {
		return 0, false
	}
	rest := err.Detail[at+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return 0, false
	}
	index, convErr := strconv.Atoi(rest[:end])
	if convErr != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// batchResponse is what the store answers to an import.
type batchResponse struct {
	Created int `json:"created"`
	Deduped int `json:"deduped"`
	Count   int `json:"count"`
}

// postBatch sends one batch to the store.
func (c *Client) postBatch(ctx context.Context, chunk []Item) (BatchResult, error) {
	body, err := json.Marshal(map[string]any{"items": chunk})
	if err != nil {
		return BatchResult{}, fmt.Errorf("the items cannot be read: %w", err)
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/api/v1/memory/items:batch", body)
	if err != nil {
		return BatchResult{}, err
	}
	if status/100 != 2 {
		return BatchResult{}, c.apiError(status, raw)
	}
	var res batchResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return BatchResult{}, errors.New("the memory store answered something that is not JSON")
	}
	out := BatchResult{Created: res.Created, Deduplicated: res.Deduped}
	if out.Created == 0 && out.Deduplicated == 0 && res.Count > 0 {
		// An older store counts the items and names them differently.
		out.Created = res.Count
	}
	return out, nil
}

// List gives the person's own items.
func (c *Client) List(ctx context.Context, scope, query string) (ListResult, error) {
	path := "/api/v1/memory/items"
	params := url.Values{}
	if scope != "" {
		params.Set("scope", scope)
	}
	if query != "" {
		params.Set("q", query)
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	raw, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ListResult{}, err
	}
	if status/100 != 2 {
		return ListResult{}, c.apiError(status, raw)
	}
	var res struct {
		Items []ListItem `json:"items"`
		Count int        `json:"count"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return ListResult{}, errors.New("the memory store answered something that is not JSON")
	}
	return ListResult{Items: res.Items, Count: res.Count}, nil
}

// do sends one request with the person's token and gives back the body and the
// status. A network failure is an error that names the cause, never the token.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if c.token == "" {
		return nil, 0, ErrNoToken
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("the request to the memory store cannot be made: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("the memory store could not be reached: %s", scrub(err.Error(), c.token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("the memory store answered badly: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// problem is the RFC 9457 body of a refusal.
type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// apiError turns a refusal into an APIError.
func (c *Client) apiError(status int, raw []byte) error {
	var p problem
	if err := json.Unmarshal(raw, &p); err != nil {
		p.Detail = strings.TrimSpace(string(raw))
	}
	detail := p.Detail
	if detail == "" {
		detail = p.Title
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	return &APIError{Status: status, Code: p.Code, Detail: scrub(detail, c.token)}
}

// scrub removes the token from a text that is about to be printed.
//
// A short token is left alone: it would match inside every other word and
// turn the message into nonsense. A real access token is long, and a long one
// is worth taking out.
func scrub(text, token string) string {
	if len(token) < MinTokenLength {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}
