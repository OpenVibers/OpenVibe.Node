package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// RobotRe matches an OpenVibe.Bot robot id (rob_ + a ULID).
var RobotRe = regexp.MustCompile(`^rob_[0-9A-Za-z]{6,64}$`)

// pairMessages turns Bot's problem codes into what the person at the terminal should do next.
var pairMessages = map[string]string{
	"bot.invalid_pairing_code": "a pairing code is 8 letters and digits, like ABCD-1234",
	"bot.pairing_code_invalid": "that is not the pairing code for this robot; check it, or ask for a new one on openvibe.bot",
	"bot.pairing_code_locked":  "too many wrong tries: the code is dead; ask for a new one on openvibe.bot",
	"bot.pairing_code_used":    "the pairing code was already used; ask for a new one on openvibe.bot",
	"bot.pairing_code_expired": "the pairing code expired (codes last 10 minutes); ask for a new one on openvibe.bot",
	"bot.no_pairing_code":      "this robot has no live pairing code; ask for a new one on openvibe.bot",
	"rate_limited":             "too many pairing attempts from here; wait a minute and try again",
}

// Pair redeems a one-time code with POST <server>/api/v1/pair.
func Pair(ctx context.Context, client *http.Client, server string, req protocol.PairRequest) (*credentials.Credentials, error) {
	if err := checkOrigin(server, "server"); err != nil {
		return nil, err
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
	resp, err := defaultClient(client).Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("pairing request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("pairing response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var pe protocol.Problem
		_ = json.Unmarshal(raw, &pe)
		if m, ok := pairMessages[pe.Code]; ok {
			return nil, errors.New(m)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, errors.New("too many pairing attempts from here; wait a minute and try again")
		}
		msg := pe.Detail
		if msg == "" {
			msg = pe.Title
		}
		return nil, fmt.Errorf("pairing refused: HTTP %d %s %s", resp.StatusCode, pe.Code, msg)
	}
	var pr protocol.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, errors.New("pairing response is not valid JSON")
	}
	if pr.DeviceID == "" || pr.Credential == "" {
		return nil, errors.New("pairing response has no device id or credential")
	}
	return &credentials.Credentials{
		DeviceID: pr.DeviceID, RobotID: pr.RobotID, Credential: credentials.NewSecret(pr.Credential),
		PublishKey: credentials.NewSecret(pr.PublishKey), WHIPURL: pr.WHIPURL, Server: strings.TrimSuffix(server, "/"),
		Profile: pr.Profile, PairedAt: time.Now().UTC(),
	}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
