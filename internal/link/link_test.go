package link

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/fakebot"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

type recorder struct {
	mu        sync.Mutex
	frames    []protocol.Frame
	connected int
	discon    []error
	onConnect func()
	gotFrame  chan protocol.Frame
	gotDown   chan error
}

func newRecorder() *recorder {
	return &recorder{gotFrame: make(chan protocol.Frame, 100), gotDown: make(chan error, 100)}
}

func (r *recorder) Connected() {
	r.mu.Lock()
	r.connected++
	f := r.onConnect
	r.mu.Unlock()
	if f != nil {
		f()
	}
}
func (r *recorder) Frame(f protocol.Frame) {
	r.mu.Lock()
	r.frames = append(r.frames, f)
	r.mu.Unlock()
	r.gotFrame <- f
}
func (r *recorder) Disconnected(err error) {
	r.mu.Lock()
	r.discon = append(r.discon, err)
	r.mu.Unlock()
	r.gotDown <- err
}

const testCred = "cred-0123456789abcdef"

// setup runs a Link against a fakebot; prep runs before the first connection.
func setup(t *testing.T, hb time.Duration, prep ...func(*fakebot.Server)) (*fakebot.Server, *Link, *recorder, *lockedWriter, context.CancelFunc) {
	t.Helper()
	srv := fakebot.New()
	t.Cleanup(srv.Close)
	cred := testCred
	srv.AddCredential(cred, "dev_test")
	for _, f := range prep {
		f(srv)
	}
	u, err := DeviceURL(srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedWriter{&bytes.Buffer{}, &sync.Mutex{}}
	rec := newRecorder()
	l := New(Options{URL: u, Credential: credentials.NewSecret(cred), HeartbeatInterval: hb, BackoffMin: 20 * time.Millisecond,
		BackoffMax: 100 * time.Millisecond, Log: slog.New(slog.NewTextHandler(logs, nil))}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return srv, l, rec, logs, cancel
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func waitFrame(t *testing.T, r *recorder, typ string) protocol.Frame {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f := <-r.gotFrame:
			if f.Type == typ {
				return f
			}
		case <-timeout:
			t.Fatalf("no %s frame", typ)
		}
	}
}

func TestConnectHelloConfigHeartbeat(t *testing.T) {
	// Bot checks the credential after the upgrade; a frame before hello would be refused with bot.not_paired.
	srv, l, rec, _, _ := setup(t, 50*time.Millisecond, func(s *fakebot.Server) { s.SetHelloDelay(200 * time.Millisecond) })
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := waitFrame(t, rec, protocol.TypeHello).Msg.(protocol.Hello)
	if h.SessionID == "" || len(h.RobotIDs) != 1 || h.RobotIDs[0] != fakebot.RobotID {
		t.Fatalf("hello %+v", h)
	}
	if _, ok := h.Time(); !ok {
		t.Fatalf("hello server_time %q", h.ServerTime)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	f, err := c.Expect(protocol.TypeHeartbeat, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.V != 1 || f.Seq == 0 || f.TS == 0 {
		t.Fatalf("envelope %+v", f.Envelope)
	}
	if srv.CredentialSeenInURL() {
		t.Fatal("credential in URL")
	}
	// Heartbeats keep the link up well past the deadman.
	time.Sleep(300 * time.Millisecond)
	if !l.Connected() {
		t.Fatal("dropped while heartbeats were acked")
	}
	if l.Stats().RTT <= 0 {
		t.Fatal("no RTT measured")
	}
	// The measured round trip goes back to the server as rtt_ms on the next heartbeat.
	if rtt, ok := c.RTT(); !ok || rtt < 0 {
		t.Fatalf("server saw no rtt_ms (%d %v)", rtt, ok)
	}
	if c.NotPaired() != 0 {
		t.Fatalf("%d frames sent before hello", c.NotPaired())
	}
	// seq increases per direction.
	var last uint64
	for _, f := range c.Frames() {
		if f.Seq <= last {
			t.Fatalf("seq not increasing: %d after %d", f.Seq, last)
		}
		last = f.Seq
	}
}

func TestDeadmanOnSilentServer(t *testing.T) {
	srv, _, rec, _, _ := setup(t, 50*time.Millisecond)
	c, _ := srv.NextConn(3 * time.Second)
	waitFrame(t, rec, protocol.TypeConfig)
	start := time.Now()
	c.Mute()
	select {
	case err := <-rec.gotDown:
		if err == nil || !strings.Contains(err.Error(), "heartbeat lost") {
			t.Fatalf("err %v", err)
		}
		if d := time.Since(start); d > 400*time.Millisecond {
			t.Fatalf("deadman took %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("silent server not detected")
	}
	// And it reconnects.
	if _, err := srv.NextConn(3 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	srv, l, rec, _, _ := setup(t, 100*time.Millisecond)
	c, _ := srv.NextConn(3 * time.Second)
	waitFrame(t, rec, protocol.TypeConfig)
	c.Close()
	<-rec.gotDown
	if err := l.Send(protocol.Heartbeat{}); !errors.Is(err, ErrOffline) && l.Connected() == false {
		t.Fatalf("send while offline: %v", err)
	}
	if _, err := srv.NextConn(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if srv.Connections() != 2 {
		t.Fatalf("connections %d", srv.Connections())
	}
}

func TestSendOffline(t *testing.T) {
	l := New(Options{URL: "ws://127.0.0.1:1/device"}, newRecorder())
	if err := l.Send(protocol.Heartbeat{}); !errors.Is(err, ErrOffline) {
		t.Fatalf("%v", err)
	}
}

// expectCredentialRefusal checks a 4002/4003 ending: a CredentialError naming the action, no fast retry (the 10 s floor;
// ordinary drops retry within BackoffMax = 100 ms here) and no credential in the logs.
func expectCredentialRefusal(t *testing.T, srv *fakebot.Server, l *Link, rec *recorder, logs *lockedWriter, cancel context.CancelFunc, code int) {
	t.Helper()
	var err error
	select {
	case err = <-rec.gotDown:
	case <-time.After(3 * time.Second):
		t.Fatal("no disconnect")
	}
	var ce *CredentialError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err %v, want a CredentialError %d", err, code)
	}
	conns := srv.Connections()
	time.Sleep(time.Second)
	if srv.Connections() != conns {
		t.Fatalf("retried within a second after close %d", code)
	}
	if !strings.Contains(l.Stats().LastError, "re-pair or update the credential") {
		t.Fatalf("last error %q", l.Stats().LastError)
	}
	cancel()
	if !strings.Contains(logs.String(), "re-pair or update the credential") || !strings.Contains(logs.String(), "retry_in=1") {
		t.Fatalf("logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), testCred) {
		t.Fatalf("credential in logs: %s", logs.String())
	}
}

func TestRevokedBacksOff(t *testing.T) {
	srv, l, rec, logs, cancel := setup(t, 100*time.Millisecond)
	if _, err := srv.NextConn(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	srv.Revoke(testCred) // closes with 4003
	expectCredentialRefusal(t, srv, l, rec, logs, cancel, protocol.CloseRevoked)
}

func TestBadCredentialBacksOff(t *testing.T) {
	// Bot upgrades first, then closes an unknown credential with 4002.
	srv, l, rec, logs, cancel := setup(t, 100*time.Millisecond, func(s *fakebot.Server) { s.Revoke(testCred) })
	expectCredentialRefusal(t, srv, l, rec, logs, cancel, protocol.CloseBadCredential)
	if srv.Connections() != 1 {
		t.Fatalf("connections %d", srv.Connections())
	}
}

func TestReplacedReconnects(t *testing.T) {
	srv, _, rec, _, _ := setup(t, 100*time.Millisecond)
	if _, err := srv.NextConn(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	// A second connection of the same device replaces the first (close 4000); the link reconnects after an ordinary
	// backoff, replacing that one in turn.
	u, _ := DeviceURL(srv.URL())
	ws, _, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + testCred}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	select {
	case err := <-rec.gotDown:
		if err == nil || !strings.Contains(err.Error(), "replaced") || IsCredentialError(err) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not replaced")
	}
	if _, err := srv.NextConn(3 * time.Second); err != nil { // the manual one
		t.Fatal(err)
	}
	if _, err := srv.NextConn(3 * time.Second); err != nil { // the link again
		t.Fatal(err)
	}
}

func TestServerErrorFrame(t *testing.T) {
	srv, l, rec, _, _ := setup(t, 100*time.Millisecond)
	c, _ := srv.NextConn(3 * time.Second)
	waitFrame(t, rec, protocol.TypeConfig)
	if err := c.SendError("bot.unknown_message", "unknown type x"); err != nil {
		t.Fatal(err)
	}
	e := waitFrame(t, rec, protocol.TypeError).Msg.(protocol.Error)
	if e.Code != "bot.unknown_message" || e.Detail != "unknown type x" || !l.Connected() {
		t.Fatalf("%+v", e)
	}
}

func TestBackoff(t *testing.T) {
	l := New(Options{URL: "ws://127.0.0.1:1/device"}, newRecorder())
	for _, err := range []error{&CredentialError{Code: protocol.CloseBadCredential}, &CredentialError{Code: protocol.CloseRevoked},
		fmt.Errorf("session: %w", &CredentialError{Code: 401})} {
		for i := 0; i < 200; i++ {
			if w := l.backoff(err, i%40); w < 10*time.Second || w > 40*time.Second {
				t.Fatalf("%v: attempt %d waits %s", err, i%40, w)
			}
		}
		if !strings.Contains(err.Error(), "re-pair or update the credential") {
			t.Fatalf("%v", err)
		}
	}
	for i := 0; i < 200; i++ {
		if w := l.backoff(errors.New("read: EOF"), 0); w < 250*time.Millisecond || w >= 750*time.Millisecond {
			t.Fatalf("first retry waits %s", w)
		}
	}
}

func TestDeviceURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://openvibe.bot":         "wss://openvibe.bot/device",
		"https://openvibe.bot/":        "wss://openvibe.bot/device",
		"http://127.0.0.1:8080":        "ws://127.0.0.1:8080/device",
		"https://x.example/base?q=1#f": "wss://x.example/base/device",
	} {
		got, err := DeviceURL(in)
		if err != nil || got != want {
			t.Errorf("%s → %s (%v), want %s", in, got, err, want)
		}
	}
	if _, err := DeviceURL("ftp://x"); err == nil {
		t.Error("ftp accepted")
	}
}

func TestPair(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCode("ABCD1234")
	req := protocol.PairRequest{Code: "ABCD1234", AgentVersion: "t", DeviceKind: "onboard", Drivers: []string{"dryrun"}, Name: "rover"}
	c, err := Pair(context.Background(), nil, srv.URL(), req)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID == "" || c.Credential.IsZero() || c.PublishKey.IsZero() || c.WHIPURL != "" {
		t.Fatalf("%+v", c)
	}
	// Bot answers a used code, without a robot, as not a live code.
	if _, err := Pair(context.Background(), nil, srv.URL(), req); err == nil || !strings.Contains(err.Error(), "not the pairing code") {
		t.Fatalf("code reused: %v", err)
	}
	if got := srv.PairRequests(); len(got) != 2 || got[0].Drivers[0] != "dryrun" || got[0].DeviceKind != "onboard" || got[0].Name != "rover" {
		t.Fatalf("%+v", got)
	}
	if _, err := Pair(context.Background(), nil, "http://openvibe.example", req); err == nil {
		t.Fatal("plain http to a remote host accepted")
	}
}

// TestPairRefusals covers Bot's problem codes: with robot sent, wrong tries are charged and the fifth kills the code.
func TestPairRefusals(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddRobotCode(fakebot.RobotID, "WXYZ7890")
	pair := func(robot, code string) error {
		_, err := Pair(context.Background(), nil, srv.URL(), protocol.PairRequest{Robot: robot, Code: code, AgentVersion: "t", DeviceKind: "onboard"})
		return err
	}
	for i := 0; i < 4; i++ {
		if err := pair(fakebot.RobotID, "AAAA0000"); err == nil || !strings.Contains(err.Error(), "not the pairing code") {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if err := pair(fakebot.RobotID, "AAAA0000"); err == nil || !strings.Contains(err.Error(), "too many wrong tries") {
		t.Fatalf("fifth try: %v", err)
	}
	if err := pair(fakebot.RobotID, "WXYZ7890"); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("dead code: %v", err)
	}
	if got := srv.PairRequests(); got[0].Robot != fakebot.RobotID {
		t.Fatalf("robot not sent: %+v", got[0])
	}
	if err := pair("rob_other", "WXYZ7890"); err == nil || !strings.Contains(err.Error(), "has no pairing code") {
		t.Fatalf("no code: %v", err)
	}
	srv.AddCode("EXPD1234")
	srv.ExpireCode("EXPD1234")
	if err := pair("", "EXPD1234"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	if err := pair("", "ABC"); err == nil || !strings.Contains(err.Error(), "8 letters") {
		t.Fatalf("shape: %v", err)
	}
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{429, `{"type":"about:blank","status":429,"code":"rate_limited","detail":"slow down"}`, "wait a minute"},
		{500, `{"title":"Internal Server Error","status":500,"code":"bot.internal","detail":"boom"}`, "HTTP 500 bot.internal: boom"},
		{502, `<html>bad gateway</html>`, "HTTP 502"},
	} {
		if err := pairRefusal(c.status, []byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: %v", c.status, err)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"abcd-1234": "ABCD1234", " WXYZ 7890 ": "WXYZ7890", "OOII-LLOO": "00111100"} {
		got, err := NormalizeCode(in)
		if err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "ABC", "ABCD-12345", "ABCU1234"} {
		if _, err := NormalizeCode(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
