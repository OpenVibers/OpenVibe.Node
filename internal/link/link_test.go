package link

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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

func setup(t *testing.T, hb time.Duration) (*fakebot.Server, *Link, *recorder, *lockedWriter, context.CancelFunc) {
	t.Helper()
	srv := fakebot.New()
	t.Cleanup(srv.Close)
	cred := "cred-0123456789abcdef"
	srv.AddCredential(cred, "dev_test")
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
	srv, l, rec, _, _ := setup(t, 50*time.Millisecond)
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeHello)
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
	// seq increases per direction.
	var last uint64
	for _, f := range c.Frames() {
		if f.Seq <= last {
			t.Fatalf("seq not increasing: %d after %d", f.Seq, last)
		}
		last = f.Seq
	}
}

// Bot's heartbeat_ack echoes the device's send time (echo/t) over its own envelope seq, so the Node measures the
// round trip from that; a fresh connection starts with no measurement.
func TestHeartbeatRTTReportedAfterAck(t *testing.T) {
	srv, l, rec, _, _ := setup(t, 60*time.Millisecond)
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeHello)
	waitFrame(t, rec, protocol.TypeConfig)

	first, err := c.Expect(protocol.TypeHeartbeat, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hb := first.Msg.(protocol.Heartbeat); hb.T == 0 {
		t.Fatalf("heartbeat carried no t: %+v", hb)
	} else if hb.RTTMS != nil {
		t.Fatalf("first heartbeat on a connection carried rtt_ms %d", *hb.RTTMS)
	}

	// fakebot auto-acks, so a later heartbeat carries the measurement: present, and never negative.
	for {
		f, err := c.Expect(protocol.TypeHeartbeat, 3*time.Second, nil)
		if err != nil {
			t.Fatal(err)
		}
		hb := f.Msg.(protocol.Heartbeat)
		if hb.RTTMS == nil {
			continue
		}
		if *hb.RTTMS < 0 {
			t.Fatalf("rtt_ms %d", *hb.RTTMS)
		}
		break
	}
	if l.Stats().RTT <= 0 {
		t.Fatal("no RTT in stats")
	}

	// A reconnect starts fresh: the first heartbeat on the new connection again carries no rtt_ms.
	c.Close()
	<-rec.gotDown
	c2, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeHello)
	waitFrame(t, rec, protocol.TypeConfig)
	first2, err := c2.Expect(protocol.TypeHeartbeat, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hb := first2.Msg.(protocol.Heartbeat); hb.RTTMS != nil {
		t.Fatalf("first heartbeat on a new connection carried rtt_ms %d", *hb.RTTMS)
	}
}

// An ack that carries only echo (Bot's newer field, no t) still sets the RTT.
func TestHeartbeatAckEchoOnlySetsRTT(t *testing.T) {
	srv, l, rec, _, _ := setup(t, 200*time.Millisecond)
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	c.Mute() // stop fakebot's automatic t+echo answers; this test sends the ack itself
	f, err := c.Expect(protocol.TypeHeartbeat, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	echo := f.Msg.(protocol.Heartbeat).T
	if echo == 0 {
		t.Fatal("heartbeat carried no t to echo")
	}
	if err := c.Send(protocol.HeartbeatAck{Echo: &echo}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Stats().RTT <= 0 {
		if time.Now().After(deadline) {
			t.Fatal("an echo-only heartbeat_ack did not set the RTT")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An ack that carries only seq (older Bot, no t/echo) still sets the RTT from the bounded send-time lookup.
func TestHeartbeatAckSeqOnlySetsRTT(t *testing.T) {
	srv, l, rec, _, _ := setup(t, 200*time.Millisecond)
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, rec, protocol.TypeConfig)
	c.Mute() // stop fakebot's automatic t+echo answers; this test sends the ack itself
	f, err := c.Expect(protocol.TypeHeartbeat, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	hb := f.Msg.(protocol.Heartbeat)
	if err := c.Send(protocol.HeartbeatAck{Seq: hb.Seq}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Stats().RTT <= 0 {
		if time.Now().After(deadline) {
			t.Fatal("a seq-only heartbeat_ack did not set the RTT")
		}
		time.Sleep(5 * time.Millisecond)
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

// A revoke closes the live socket with 4003: the link says to pair again, keeps trying no sooner than
// CredentialRetryMin, and never logs the credential.
func TestRevokedRetriesAfterTenSecondsAndNeverLogsCredential(t *testing.T) {
	srv, l, rec, logs, cancel := setup(t, 100*time.Millisecond)
	waitFrame(t, rec, protocol.TypeConfig)
	srv.Revoke("cred-0123456789abcdef")
	if err := <-rec.gotDown; err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("disconnect error %v", err)
	}
	waitLastError(t, l, "pair again")
	if !strings.Contains(l.Stats().LastError, "current credential") {
		t.Fatalf("last error %q does not say how to recover", l.Stats().LastError)
	}
	time.Sleep(time.Second) // ten times BackoffMax: an ordinary reconnect would have dialed again by now
	if n := srv.Dials(); n != 1 {
		t.Fatalf("dialed %d times within a second of a revoke", n)
	}
	cancel()
	if strings.Contains(logs.String(), "cred-0123456789abcdef") {
		t.Fatalf("credential in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "refused the device credential") {
		t.Fatalf("no credential diagnosis in the log: %s", logs.String())
	}
}

// A credential the server does not know is closed with 4002 right after the upgrade: the link reports it and waits
// at least CredentialRetryMin before the next try instead of giving up or hammering the server.
func TestRefusedCredentialWaitsTenSeconds(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	u, _ := DeviceURL(srv.URL())
	rec := newRecorder()
	l := New(Options{URL: u, Credential: credentials.NewSecret("cred-unknown"), BackoffMin: 10 * time.Millisecond,
		BackoffMax: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitLastError(t, l, "pair again")
	time.Sleep(time.Second)
	if n := srv.Dials(); n != 1 {
		t.Fatalf("dialed %d times within a second of a 4002", n)
	}
	select {
	case <-done:
		t.Fatal("the link gave up on a refused credential")
	default:
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.connected != 0 {
		t.Fatalf("connected %d times with a refused credential", rec.connected)
	}
}

// The wait after 4002 / 4003 is never under CredentialRetryMin, whatever the jitter draws; other errors keep the
// short exponential backoff.
func TestCredentialBackoffFloor(t *testing.T) {
	l := New(Options{URL: "ws://127.0.0.1:1/device", BackoffMin: time.Millisecond, BackoffMax: 20 * time.Second}, newRecorder())
	for _, err := range []error{closeErr(&websocket.CloseError{Code: protocol.CloseInvalid}), closeErr(&websocket.CloseError{Code: protocol.CloseRevoked}), errUnauthorized} {
		if !credentialRefused(err) {
			t.Fatalf("%v is not a credential refusal", err)
		}
		for attempt := 0; attempt < 40; attempt++ {
			for i := 0; i < 50; i++ {
				if w := l.backoff(err, attempt); w < CredentialRetryMin || w > CredentialRetryMin+20*time.Second {
					t.Fatalf("attempt %d after %v: wait %s", attempt, err, w)
				}
			}
		}
	}
	replaced := closeErr(&websocket.CloseError{Code: protocol.CloseReplaced})
	if replaced == nil || credentialRefused(replaced) {
		t.Fatalf("4000 maps to %v", replaced)
	}
	for i := 0; i < 200; i++ {
		if w := l.backoff(replaced, 0); w > 2*time.Millisecond {
			t.Fatalf("4000 waited %s on the first attempt", w)
		}
	}
}

func waitLastError(t *testing.T, l *Link, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(l.Stats().LastError, want) {
		if time.Now().After(deadline) {
			t.Fatalf("last error %q, want %q", l.Stats().LastError, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A second connection with the same credential replaces the first (4000); the replaced link reconnects with its
// normal backoff, as Bot expects of a device whose stale socket was replaced.
func TestReplacedReconnects(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCredential("cred-x", "dev_x")
	u, _ := DeviceURL(srv.URL())
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecorder()
	first := New(Options{URL: u, Credential: credentials.NewSecret("cred-x"), BackoffMin: 10 * time.Millisecond,
		BackoffMax: 50 * time.Millisecond, Log: quiet}, rec)
	go first.Run(ctx)
	waitFrame(t, rec, protocol.TypeConfig)
	second := New(Options{URL: u, Credential: credentials.NewSecret("cred-x"), BackoffMin: time.Hour, Log: quiet}, newRecorder())
	go second.Run(ctx)
	if err := <-rec.gotDown; err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("err %v", err)
	}
	// The first link comes back within its normal backoff (and replaces the second in turn).
	waitFrame(t, rec, protocol.TypeHello)
	if n := srv.Connections(); n != 3 {
		t.Fatalf("connections %d", n)
	}
}

// ServerNow reads the server's clock from the frames' ts: the skew less the fastest transit.
func TestServerNow(t *testing.T) {
	l := New(Options{URL: "ws://127.0.0.1:1/device"}, newRecorder())
	if _, ok := l.ServerNow(time.Now()); ok {
		t.Fatal("an estimate before any frame")
	}
	l.observe(10_000, time.UnixMilli(9_900))  // server 100 ms ahead, fast frame
	l.observe(10_000, time.UnixMilli(10_500)) // a frame that sat 600 ms in a queue
	if ms, ok := l.ServerNow(time.UnixMilli(20_000)); !ok || ms != 20_100 {
		t.Fatalf("server now %d %v", ms, ok)
	}
	// A long run of frames that each sat 500 ms on the way never displaces the timely one: an expired deadline must
	// not read as open again.
	for i := 0; i < 100; i++ {
		l.observe(int64(30_000+i*10), time.UnixMilli(int64(30_000+i*10)+400))
	}
	if ms, _ := l.ServerNow(time.UnixMilli(40_000)); ms != 40_100 {
		t.Fatalf("delayed frames moved the estimate: %d", ms)
	}
	// A new connection starts a new estimate.
	l.mu.Lock()
	l.hasSkew = false
	l.mu.Unlock()
	l.observe(50_000, time.UnixMilli(50_000))
	if ms, _ := l.ServerNow(time.UnixMilli(60_000)); ms != 60_000 {
		t.Fatalf("estimate across connections: %d", ms)
	}
}

// Before hello the socket is not authenticated: nothing is sent and Connected has not run.
func TestNothingSentBeforeHello(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCredential("cred-x", "dev_x")
	srv.Greeting = [][]byte{} // upgrade, authenticate, but stay silent
	u, _ := DeviceURL(srv.URL())
	rec := newRecorder()
	l := New(Options{URL: u, Credential: credentials.NewSecret("cred-x"), HeartbeatInterval: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	c, err := srv.NextConn(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if len(c.Frames()) != 0 || rec.connected != 0 || l.Connected() {
		t.Fatalf("sent %d frames before hello", len(c.Frames()))
	}
	if err := l.Send(protocol.Heartbeat{}); !errors.Is(err, ErrOffline) {
		t.Fatalf("send before hello: %v", err)
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
	req := protocol.PairRequest{Robot: fakebot.RobotID, Code: "ABCD1234", AgentVersion: "t", DeviceKind: "onboard", Drivers: []string{"dryrun"}}
	c, err := Pair(context.Background(), nil, srv.URL(), req)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID == "" || c.Credential.IsZero() || c.PublishKey.IsZero() || c.RobotID != fakebot.RobotID {
		t.Fatalf("%+v", c)
	}
	if _, err := Pair(context.Background(), nil, srv.URL(), req); err == nil || !strings.Contains(err.Error(), "not the pairing code") {
		t.Fatalf("code reused: %v", err)
	}
	if got := srv.PairRequests(); len(got) != 2 || got[0].Drivers[0] != "dryrun" || got[0].DeviceKind != "onboard" || got[0].Robot != fakebot.RobotID {
		t.Fatalf("%+v", got)
	}
	if _, err := Pair(context.Background(), nil, "http://openvibe.example", req); err == nil {
		t.Fatal("plain http to a remote host accepted")
	}
}

// Each of Bot's pairing refusals (RFC 9457 problem bodies from pairing.redeem and the rate limiter) becomes a message
// that says what to do next; anything else keeps Bot's status, code and detail.
func TestPairRefusals(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	req := protocol.PairRequest{Robot: fakebot.RobotID, Code: "ABCD1234", AgentVersion: "t", DeviceKind: "onboard"}
	for _, tc := range []struct {
		status       int
		code, detail string
		want         string
	}{
		{403, "bot.pairing_code_invalid", "that is not the pairing code", "not the pairing code for this robot"},
		{403, "bot.pairing_code_locked", "too many wrong tries; the code is dead", "the code is dead"},
		{403, "bot.pairing_code_used", "that pairing code has already been used", "already used"},
		{403, "bot.pairing_code_expired", "that pairing code has expired", "codes last 10 minutes"},
		{404, "bot.no_pairing_code", "this robot has no pairing code; ask the owner for a new one", "no live pairing code"},
		{422, "bot.invalid_pairing_code", "the pairing code must be 8 characters (XXXX-XXXX)", "like ABCD-1234"},
		{429, "rate_limited", "", "wait a minute"},
		{422, "bot.invalid_input", "drivers must be a list", "HTTP 422 bot.invalid_input drivers must be a list"},
	} {
		srv.RefusePair(tc.status, tc.code, tc.detail)
		_, err := Pair(context.Background(), nil, srv.URL(), req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.code, err, tc.want)
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
