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
		return nil, pairRefusal(resp.StatusCode, raw)
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

// pairRefusal turns Bot's RFC 9457 problem (code, detail) into a message that says what to do next.
func pairRefusal(status int, raw []byte) error {
	var p protocol.Problem
	_ = json.Unmarshal(raw, &p)
	switch p.Code {
	case protocol.ProblemCodeInvalid:
		return errors.New("that is not the pairing code; check it (5 wrong tries end a code) or ask the owner for a new one on openvibe.bot")
	case protocol.ProblemCodeLocked:
		return errors.New("too many wrong tries: this pairing code is dead; ask the owner for a new one on openvibe.bot")
	case protocol.ProblemCodeUsed:
		return errors.New("this pairing code has already been used; ask the owner for a new one on openvibe.bot")
	case protocol.ProblemCodeExpired:
		return errors.New("this pairing code has expired (codes last 10 minutes); ask the owner for a new one on openvibe.bot")
	case protocol.ProblemNoCode:
		return errors.New("this robot has no pairing code; ask the owner for a new one on openvibe.bot")
	case protocol.ProblemCodeShape:
		return errors.New("a pairing code is 8 letters and digits, like ABCD-1234")
	}
	if status == http.StatusTooManyRequests || p.Code == protocol.ProblemRateLimited {
		return errors.New("too many pairing attempts from this address; wait a minute and try again")
	}
	msg := p.Detail
	if msg == "" {
		msg = p.Error
	}
	if msg == "" {
		msg = p.Title
	}
	if p.Code != "" {
		return fmt.Errorf("pairing refused: HTTP %d %s: %s", status, p.Code, msg)
	}
	return fmt.Errorf("pairing refused: HTTP %d %s", status, msg)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
