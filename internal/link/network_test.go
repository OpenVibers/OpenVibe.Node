package link

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakenetwork"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

func networkPair(t *testing.T) (*fakebot.Server, *fakenetwork.Server, *credentials.Credentials) {
	t.Helper()
	bot := fakebot.New()
	t.Cleanup(bot.Close)
	net := fakenetwork.New(bot)
	t.Cleanup(net.Close)
	net.AddCode("ABCD2345")
	c, err := PairNetwork(context.Background(), nil, net.URL()+"/",
		protocol.NodePairingRequest{Code: "ABCD2345", Pairing: fakenetwork.PairingID, Name: "Rover"}, "openvibe-node/t")
	if err != nil {
		t.Fatal(err)
	}
	c.Server = bot.URL()
	return bot, net, c
}

// The wire sequence: node-pairing → oauth/token (client_credentials, audience openvibe.bot) → devices/bind with the
// token; the bind answer fills the device fields exactly as POST /pair does.
func TestPairNetworkTokenAndBind(t *testing.T) {
	bot, net, c := networkPair(t)
	if c.Principal != fakenetwork.Principal || c.NodeCredential.Reveal() != net.Credential() || c.Network != net.URL() ||
		c.PairedFor == nil || c.PairedFor.Service != "bot" || c.RobotID != fakebot.RobotID || c.Bound() || !c.NetworkPaired() {
		t.Fatalf("%+v", c)
	}
	if p := net.Pairings(); len(p) != 1 || p[0].Code != "ABCD2345" || p[0].Pairing != fakenetwork.PairingID || p[0].Name != "Rover" {
		t.Fatalf("%+v", p)
	}
	ts := NewTokenSource(c, nil, "openvibe-node/t")
	bound, err := Bind(context.Background(), nil, c, ts, "openvibe-node/t")
	if err != nil {
		t.Fatal(err)
	}
	if bound.DeviceID != fakenetwork.DeviceID || bound.RobotID != fakebot.RobotID || bound.PublishKey.IsZero() ||
		!strings.Contains(bound.WHIPURL, bound.PublishKey.Reveal()) || len(bound.Profile) == 0 || bound.Principal != c.Principal {
		t.Fatalf("%+v", bound)
	}
	forms := net.TokenForms()
	if len(forms) != 1 || forms[0]["grant_type"] != "client_credentials" || forms[0]["client_id"] != fakenetwork.Principal ||
		forms[0]["client_secret"] != net.Credential() || forms[0]["audience"] != "openvibe.bot" {
		t.Fatalf("%+v", forms)
	}
	// Cached: a second Token is the same, with no new request.
	if tok, _ := ts.Token(context.Background()); tok.Reveal() != net.Tokens()[0] || len(net.TokenForms()) != 1 {
		t.Fatal("token not cached")
	}
	if bot.Binds() != 1 {
		t.Fatalf("binds %d", bot.Binds())
	}
}

func TestTokenCachedUntilExpiryMinusSkew(t *testing.T) {
	_, net, c := networkPair(t)
	ts := NewTokenSource(c, nil, "t")
	now := time.Now()
	ts.now = func() time.Time { return now }
	a, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(300*time.Second - TokenSkew - time.Second)
	if b, _ := ts.Token(context.Background()); b.Reveal() != a.Reveal() {
		t.Fatal("refetched before expiry minus skew")
	}
	now = now.Add(2 * time.Second)
	if b, _ := ts.Token(context.Background()); b.Reveal() == a.Reveal() || len(net.Tokens()) != 2 {
		t.Fatal("not refetched at expiry minus skew")
	}
}

// A revoked principal: /oauth/token answers 401 invalid_client, which is ErrNodeRevoked, and the error never quotes
// the credential.
func TestTokenRevoked(t *testing.T) {
	_, net, c := networkPair(t)
	net.Revoke()
	_, err := NewTokenSource(c, nil, "t").Token(context.Background())
	if !errors.Is(err, ErrNodeRevoked) || strings.Contains(err.Error(), net.Credential()) || !strings.Contains(err.Error(), "pair again") {
		t.Fatalf("%v", err)
	}
	if _, err := Bind(context.Background(), nil, c, NewTokenSource(c, nil, "t"), "t"); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("bind: %v", err)
	}
}

// Bot answering 401 to the bind refetches the token once, and only once.
func TestBindRefetchesTokenOnceOn401(t *testing.T) {
	bot, net, c := networkPair(t)
	bot.RefuseBind(http.StatusUnauthorized, "bot.node_token_required", "")
	if _, err := Bind(context.Background(), nil, c, NewTokenSource(c, nil, "t"), "t"); err != nil {
		t.Fatal(err)
	}
	if bot.Binds() != 2 || len(net.Tokens()) != 2 {
		t.Fatalf("binds %d tokens %d", bot.Binds(), len(net.Tokens()))
	}
	bot.RefuseBind(http.StatusUnauthorized, "bot.node_token_required", "")
	bot.RefuseBind(http.StatusUnauthorized, "bot.node_token_required", "")
	_, err := Bind(context.Background(), nil, c, NewTokenSource(c, nil, "t"), "t")
	if err == nil || !strings.Contains(err.Error(), "node token") || bot.Binds() != 4 {
		t.Fatalf("%v binds %d", err, bot.Binds())
	}
	bot.RefuseBind(http.StatusForbidden, "bot.node_not_bound", "")
	if _, err := Bind(context.Background(), nil, c, NewTokenSource(c, nil, "t"), "t"); err == nil || !strings.Contains(err.Error(), "will not bind") {
		t.Fatalf("%v", err)
	}
}

func TestPairNetworkRefusals(t *testing.T) {
	net := fakenetwork.New(nil)
	defer net.Close()
	_, err := PairNetwork(context.Background(), nil, net.URL(), protocol.NodePairingRequest{Code: "ABCD2345"}, "t")
	if err == nil || !strings.Contains(err.Error(), "not the pairing code") {
		t.Fatalf("%v", err)
	}
	if _, err := PairNetwork(context.Background(), nil, "http://example.com", protocol.NodePairingRequest{Code: "ABCD2345"}, "t"); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("%v", err)
	}
}

// A Network-paired link presents node tokens, sends reauth with a fresh one, and stops (Run returns ErrNodeRevoked)
// once Network refuses the credential. Nothing secret reaches the log.
func TestNodeTokenLinkReauthAndRevoked(t *testing.T) {
	bot, net, c := networkPair(t)
	u, _ := DeviceURL(bot.URL())
	var logs bytes.Buffer
	lw := &lockedWriter{&logs, &sync.Mutex{}}
	rec := newRecorder()
	l := New(Options{URL: u, Tokens: NewTokenSource(c, nil, "t"), ReauthInterval: 200 * time.Millisecond, HeartbeatInterval: 100 * time.Millisecond,
		BackoffMin: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(lw, nil))}, rec)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- l.Run(ctx) }()
	conn, err := bot.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	f, err := conn.Expect(protocol.TypeReauth, 3*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	toks := net.Tokens()
	if len(toks) < 2 || f.Msg.(protocol.Reauth).Token != toks[len(toks)-1] {
		t.Fatalf("reauth %+v, tokens %d", f.Msg, len(toks))
	}
	net.Revoke()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNodeRevoked) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on a revoked credential")
	}
	if !strings.Contains(l.Stats().LastError, "pair again") {
		t.Fatalf("last error %q", l.Stats().LastError)
	}
	out := lw.String()
	for _, secret := range append(net.Tokens(), net.Credential()) {
		if strings.Contains(out, secret) {
			t.Fatalf("secret in logs: %s", out)
		}
	}
}
