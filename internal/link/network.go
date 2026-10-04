package link

// The Network pairing (ADR-043 B3 v2): the machine redeems a one-time code on OpenVibe.Network for a node principal
// (nod_…) and its node credential, buys short-lived node tokens with that credential, and presents a token (audience
// openvibe.bot) to Bot: once to POST /api/v1/devices/bind for its device and WHIP publish key, then on every /device
// upgrade and in `reauth` frames. Neither the code, the credential nor a token is ever logged or put in an error.

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
	"sync"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// PairingIDRe matches a Network pairing id (pair_ + a ULID), the --pairing of the installer command.
var PairingIDRe = regexp.MustCompile(`^pair_[0-9A-HJKMNP-TV-Z]{26}$`)

// principalRe matches a node principal (nod_ + a ULID).
var principalRe = regexp.MustCompile(`^nod_[0-9A-HJKMNP-TV-Z]{26}$`)

// ErrNodeRevoked is Network refusing the node credential at /oauth/token (401 invalid_client): the machine was
// revoked or removed. Retrying cannot help, so the Node stops and says how to pair again.
var ErrNodeRevoked = errors.New("OpenVibe.Network refused this machine's node credential (revoked or removed): " +
	"get a new installer command on openvibe.bot and pair again with `sudo openvibe-node pair --force --network <URL> --pairing <pair_…> --code <CODE>`")

// TokenSkew is how long before its expiry a cached node token is replaced.
const TokenSkew = 30 * time.Second

// networkPairMessages turns Network's problem codes into what the person at the terminal should do next.
var networkPairMessages = map[string]string{
	"registry.invalid_pairing_code":    "a pairing code is 8 letters and digits, like ABCD-1234",
	"registry.invalid_pairing_request": "the pairing id or name is not valid; copy the installer command from openvibe.bot again",
	"registry.pairing_code_invalid":    "that is not the pairing code for this robot; check it, or ask for a new one on openvibe.bot",
	"registry.pairing_code_locked":     "too many wrong tries: the code is dead; ask for a new one on openvibe.bot",
	"registry.pairing_code_used":       "the pairing code was already used; ask for a new one on openvibe.bot",
	"registry.pairing_code_expired":    "the pairing code expired (codes last 10 minutes); ask for a new one on openvibe.bot",
}

// checkOrigin accepts an https:// origin, or plain http to a loopback host (tests and local servers).
func checkOrigin(origin, what string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s %q is not an https:// origin", what, origin)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("refusing to talk to %s over plain http", u.Host)
	}
	return nil
}

func defaultClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Timeout: 20 * time.Second}
	}
	return c
}

// PairNetwork redeems a Network pairing code with POST <network>/api/v1/node-pairing. The result holds the node
// principal and credential but no device yet: Bind fills that in.
func PairNetwork(ctx context.Context, client *http.Client, network string, req protocol.NodePairingRequest, userAgent string) (*credentials.Credentials, error) {
	network = strings.TrimSuffix(network, "/")
	if err := checkOrigin(network, "network"); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, network+protocol.NodePairingPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", userAgent)
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
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, errors.New("too many pairing attempts from here; wait a minute and try again")
		}
		var pe protocol.Problem
		_ = json.Unmarshal(raw, &pe)
		if m, ok := networkPairMessages[pe.Code]; ok {
			return nil, errors.New(m)
		}
		return nil, fmt.Errorf("pairing refused: HTTP %d %s %s", resp.StatusCode, pe.Code, problemText(pe))
	}
	var pr protocol.NodePairingResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, errors.New("pairing response is not valid JSON")
	}
	if !principalRe.MatchString(pr.Principal) || pr.Credential == "" {
		return nil, errors.New("pairing response has no node principal or credential")
	}
	c := &credentials.Credentials{Principal: pr.Principal, NodeCredential: credentials.NewSecret(pr.Credential), Network: network,
		PairedFor: pr.PairedFor, PairedAt: time.Now().UTC()}
	if pr.PairedFor != nil {
		c.RobotID = pr.PairedFor.Ref
	}
	return c, nil
}

func problemText(pe protocol.Problem) string {
	if pe.Detail != "" {
		return pe.Detail
	}
	return pe.Title
}

// Audience is the node-token audience for the service the machine was paired for: openvibe.<service>, Bot when
// Network did not say.
func Audience(c *credentials.Credentials) string {
	if c.PairedFor != nil && c.PairedFor.Service != "" {
		return "openvibe." + c.PairedFor.Service
	}
	return "openvibe.bot"
}

// TokenSource buys node tokens at POST <network>/oauth/token and caches one until TokenSkew before it expires.
type TokenSource struct {
	network    string
	principal  string
	credential credentials.Secret
	audience   string
	client     *http.Client
	userAgent  string
	now        func() time.Time

	mu  sync.Mutex
	tok credentials.Secret
	exp time.Time
}

// NewTokenSource returns the token source of a Network-paired machine.
func NewTokenSource(c *credentials.Credentials, client *http.Client, userAgent string) *TokenSource {
	return &TokenSource{network: strings.TrimSuffix(c.Network, "/"), principal: c.Principal, credential: c.NodeCredential,
		audience: Audience(c), client: defaultClient(client), userAgent: userAgent, now: time.Now}
}

// Invalidate drops the cached token, so the next Token fetches a fresh one (after a 401 from Bot, before a reauth).
func (s *TokenSource) Invalidate() {
	s.mu.Lock()
	s.tok, s.exp = credentials.Secret{}, time.Time{}
	s.mu.Unlock()
}

// Token returns a node token, cached or fresh. A refused credential is ErrNodeRevoked.
func (s *TokenSource) Token(ctx context.Context) (credentials.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tok.IsZero() && s.now().Before(s.exp) {
		return s.tok, nil
	}
	if err := checkOrigin(s.network, "network"); err != nil {
		return credentials.Secret{}, err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {s.principal},
		"client_secret": {s.credential.Reveal()}, "audience": {s.audience}}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.network+protocol.TokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return credentials.Secret{}, err
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", s.userAgent)
	resp, err := s.client.Do(hreq)
	if err != nil {
		return credentials.Secret{}, fmt.Errorf("node token request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return credentials.Secret{}, fmt.Errorf("node token response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return credentials.Secret{}, ErrNodeRevoked
	case resp.StatusCode/100 != 2:
		var oe protocol.OAuthError
		_ = json.Unmarshal(raw, &oe)
		return credentials.Secret{}, fmt.Errorf("node token refused: HTTP %d %s %s", resp.StatusCode, oe.Error, oe.Description)
	}
	var tr protocol.TokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return credentials.Secret{}, errors.New("node token response has no access_token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	s.tok, s.exp = credentials.NewSecret(tr.AccessToken), s.now().Add(max(ttl-TokenSkew, ttl/2))
	return s.tok, nil
}

// bindMessages turns Bot's refusals of POST /api/v1/devices/bind into what to do next.
var bindMessages = map[string]string{
	"bot.node_token_required": "Bot did not take the node token (audience openvibe.bot)",
	"bot.node_not_bound":      "Bot will not bind this machine (revoked there, paired for another service or for someone else's robot): pair it again with a new code from openvibe.bot",
}

// Bind calls POST <bot>/api/v1/devices/bind with a node token and returns c with the answer's device fields (device
// id, robot, WHIP publish key and URL, profile), exactly as Pair returns them. A 401 refetches the token once.
// Every call issues a new publish key (the old one stops working), so bind only when the device record is missing.
func Bind(ctx context.Context, client *http.Client, c *credentials.Credentials, tokens *TokenSource, userAgent string) (*credentials.Credentials, error) {
	server := strings.TrimSuffix(c.Server, "/")
	if err := checkOrigin(server, "server"); err != nil {
		return nil, err
	}
	client = defaultClient(client)
	for attempt := 0; ; attempt++ {
		tok, err := tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, server+protocol.BindPath, nil)
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Authorization", "Bearer "+tok.Reveal())
		hreq.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(hreq)
		if err != nil {
			return nil, fmt.Errorf("bind request failed: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bind response: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			tokens.Invalidate()
			continue
		}
		if resp.StatusCode/100 != 2 {
			var pe protocol.Problem
			_ = json.Unmarshal(raw, &pe)
			if m, ok := bindMessages[pe.Code]; ok {
				return nil, errors.New(m)
			}
			return nil, fmt.Errorf("bind refused: HTTP %d %s %s", resp.StatusCode, pe.Code, problemText(pe))
		}
		var pr protocol.PairResponse
		if err := json.Unmarshal(raw, &pr); err != nil {
			return nil, errors.New("bind response is not valid JSON")
		}
		if pr.DeviceID == "" {
			return nil, errors.New("bind response has no device id")
		}
		next := *c
		next.Server = server
		next.DeviceID, next.RobotID, next.PublishKey, next.WHIPURL, next.Profile =
			pr.DeviceID, pr.RobotID, credentials.NewSecret(pr.PublishKey), pr.WHIPURL, pr.Profile
		return &next, nil
	}
}
