package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// maxRedirects is how many redirects a download follows (Media answers one: to the object's signed URL).
const maxRedirects = 3

// mediaClient is the OpenVibe.Media objects client of one job (worker.media; docs/worker.md). Its token is read from
// the environment variable worker.media.token_env names and goes only in the Authorization header of a request to the
// endpoint's own host: never in a job's environment, a log line or an error.
type mediaClient struct {
	cfg   config.MediaConfig
	base  *url.URL // the endpoint
	token string
	http  *http.Client
}

// newMediaClient returns the client of a job that must end by deadline (its ttl from receipt): every request is
// bounded by it. It fails when worker.media is off or the token variable is unset, so a job's inputs are never
// skipped and its result never dropped quietly.
func newMediaClient(cfg config.MediaConfig, rt http.RoundTripper, deadline time.Time) (*mediaClient, error) {
	if !cfg.Enabled {
		return nil, errors.New("worker.media is off")
	}
	cfg = cfg.WithDefaults()
	token := os.Getenv(cfg.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("worker.media: %s is not set", cfg.TokenEnv)
	}
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, errors.New("worker.media.endpoint is not an https URL")
	}
	left := time.Until(deadline)
	if left <= 0 {
		return nil, errors.New("worker.media: the job's ttl has passed")
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	c := &mediaClient{cfg: cfg, base: base, token: token}
	c.http = &http.Client{Transport: rt, Timeout: left, CheckRedirect: c.redirect}
	return c, nil
}

// redirect re-checks every hop of a download: https only, at most maxRedirects, and the token is sent to the
// endpoint's own host only (a signed URL on a storage host carries its own authority). The bytes at the end are
// checked against the pinned digest whatever host served them.
func (c *mediaClient) redirect(req *http.Request, via []*http.Request) error {
	if via[0].Method != http.MethodGet { // a redirected POST arrives here as a GET
		return errors.New("worker.media: an upload was redirected")
	}
	if len(via) > maxRedirects {
		return errors.New("worker.media: too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("worker.media: a redirect left https")
	}
	if req.URL.Host != c.base.Host {
		req.Header.Del("Authorization")
	} else {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return nil
}

// objectsURL is {endpoint}/api/v2/{app}/objects followed by the path elements given, each escaped.
func (c *mediaClient) objectsURL(elem ...string) string {
	p := []string{"api", "v2", url.PathEscape(c.cfg.App), "objects"}
	for _, e := range elem {
		p = append(p, url.PathEscape(e))
	}
	return c.base.JoinPath(p...).String()
}

// do sends req with the token and returns the response of a 2xx answer. An error names the status or the transport's
// cause, never a URL (a signed URL is a credential too) and never the token.
func (c *mediaClient) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp, nil
}

// fetchInput downloads one digest-pinned input into dir as in.Name (protocol.ValidJobInputs made it a plain file
// name), reading at most limit bytes while streaming: a body over limit, a size other than the one declared, or a
// sha256 other than the pinned one returns an error, and the caller ends the job failed before its process starts.
func (c *mediaClient) fetchInput(ctx context.Context, in protocol.JobInput, dir string, limit int64) (int64, error) {
	fail := func(err error) (int64, error) { return 0, fmt.Errorf("input %s (%s): %w", in.Name, in.MediaID, err) }
	if !protocol.ValidMediaID(in.MediaID) || filepath.Base(in.Name) != in.Name || in.Name == "." || in.Name == ".." {
		return fail(errors.New("malformed"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectsURL(in.MediaID, "download")+"?redirect=1", nil)
	if err != nil {
		return fail(err)
	}
	resp, err := c.do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Scheme != "https" { // CheckRedirect refuses it; never trust a single check on this path
		return fail(errors.New("not served over https"))
	}
	if resp.ContentLength > limit {
		return fail(fmt.Errorf("%d bytes, over the %d allowed", resp.ContentLength, limit))
	}
	f, err := os.OpenFile(filepath.Join(dir, in.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return fail(err)
	case n > limit:
		return fail(fmt.Errorf("over the %d bytes allowed", limit))
	case in.SizeBytes > 0 && n != in.SizeBytes:
		return fail(fmt.Errorf("%d bytes, not the %d declared", n, in.SizeBytes))
	case hex.EncodeToString(h.Sum(nil)) != in.SHA256:
		return fail(errors.New("sha256 does not match"))
	}
	return n, nil
}

// uploadResult stores data, a job's JSON result, as a private Media object (init, PUT content, complete, each to the
// endpoint, the upload never redirected) and returns its id.
func (c *mediaClient) uploadResult(ctx context.Context, jobID string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	send := func(method, u string, body []byte, ctype string, out any) error {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", ctype)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if out == nil {
			return nil
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	created, _ := json.Marshal(map[string]any{"kind": "file", "visibility": "private", "size_bytes": len(data),
		"content_hash": digest, "mime_type": "application/json", "filename": jobID + ".json",
		"metadata": map[string]string{"job_id": jobID}})
	var made struct {
		ID string `json:"id"`
	}
	if err := send(http.MethodPost, c.objectsURL(), created, "application/json", &made); err != nil {
		return "", fmt.Errorf("result upload: init: %w", err)
	}
	if !protocol.ValidMediaID(made.ID) {
		return "", errors.New("result upload: init answered no object id")
	}
	var put struct {
		ContentHash string `json:"content_hash"`
	}
	if err := send(http.MethodPut, c.objectsURL(made.ID, "content"), data, "application/json", &put); err != nil {
		return "", fmt.Errorf("result upload: content: %w", err)
	}
	if put.ContentHash != digest {
		return "", errors.New("result upload: Media stored other bytes")
	}
	done, _ := json.Marshal(map[string]string{"content_hash": digest})
	if err := send(http.MethodPost, c.objectsURL(made.ID, "complete"), done, "application/json", nil); err != nil {
		return "", fmt.Errorf("result upload: complete: %w", err)
	}
	return made.ID, nil
}
