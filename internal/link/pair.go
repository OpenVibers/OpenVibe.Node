package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Pairing codes are 8 Crockford base32 characters, shown as XXXX-XXXX (ADR-043 decision 2).
var codeRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)

// NormalizeCode upper-cases the code, drops the dash and spaces, and maps the Crockford look-alikes (I, L → 1; O → 0).
func NormalizeCode(code string) (string, error) {
	c := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	c = strings.NewReplacer("I", "1", "L", "1", "O", "0").Replace(c)
	if !codeRe.MatchString(c) {
		return "", errors.New("a pairing code is 8 letters and digits, like ABCD-1234")
	}
	return c, nil
}

// Pair redeems a one-time code at <server>/api/v1/pair.
func Pair(ctx context.Context, client *http.Client, server string, req protocol.PairRequest) (*credentials.Credentials, error) {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("server %q is not an https:// origin", server)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("refusing to pair over plain http with %s", u.Host)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(server, "/") + protocol.PairPath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", "openvibe-node/"+req.AgentVersion)
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("pairing request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("pairing response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var pe protocol.PairError
		_ = json.Unmarshal(raw, &pe)
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || pe.Error == "invalid_code" || pe.Error == "expired":
			return nil, errors.New("the pairing code is wrong, used or expired; ask for a new one on openvibe.bot")
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, errors.New("too many wrong tries for this code; ask for a new one on openvibe.bot")
		}
		msg := pe.Message
		if msg == "" {
			msg = pe.Error
		}
		return nil, fmt.Errorf("pairing refused: HTTP %d %s", resp.StatusCode, msg)
	}
	var pr protocol.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, errors.New("pairing response is not valid JSON")
	}
	if pr.DeviceID == "" || pr.Credential == "" {
		return nil, errors.New("pairing response has no device id or credential")
	}
	return &credentials.Credentials{
		DeviceID: pr.DeviceID, Credential: credentials.NewSecret(pr.Credential), PublishKey: credentials.NewSecret(pr.PublishKey),
		Server: strings.TrimSuffix(server, "/"), DeviceURL: pr.DeviceURL, WHIPURL: pr.WHIPURL, ICEServers: pr.ICEServers,
		Profile: pr.Profile, PairedAt: time.Now().UTC(),
	}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
