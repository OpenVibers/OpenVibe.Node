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

// A revoke closes the live socket with 4003: the link stops for good and never logs the credential.
func TestRevokedStopsAndNeverLogsCredential(t *testing.T) {
	srv, l, rec, logs, cancel := setup(t, 100*time.Millisecond)
	waitFrame(t, rec, protocol.TypeConfig)
	srv.Revoke("cred-0123456789abcdef")
	<-rec.gotDown
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(l.Stats().LastError, "pair again") {
		if time.Now().After(deadline) {
			t.Fatalf("last error %q", l.Stats().LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // well past BackoffMax: a running loop would have dialed again
	if n := srv.Connections(); n != 1 {
		t.Fatalf("reconnected after a revoke: %d connections", n)
	}
	cancel()
	if strings.Contains(logs.String(), "cred-0123456789abcdef") {
		t.Fatalf("credential in logs: %s", logs.String())
	}
}

// A credential the server does not know is closed with 4002 after the upgrade; the link gives up after MaxRefused.
func TestRefusedCredentialGivesUp(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	u, _ := DeviceURL(srv.URL())
	rec := newRecorder()
	l := New(Options{URL: u, Credential: credentials.NewSecret("cred-unknown"), BackoffMin: 10 * time.Millisecond, MaxRefused: 1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rec)
	done := make(chan struct{})
	go func() { l.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("link kept dialing with a refused credential")
	}
	if !strings.Contains(l.Stats().LastError, "pair again") || rec.connected != 0 {
		t.Fatalf("last error %q, connected %d", l.Stats().LastError, rec.connected)
	}
}

// A second connection with the same credential replaces the first (4000); the replaced link waits ReplacedBackoff
// instead of dialing straight back and kicking the newcomer off.
func TestReplacedWaitsBeforeReconnecting(t *testing.T) {
	srv := fakebot.New()
	defer srv.Close()
	srv.AddCredential("cred-x", "dev_x")
	u, _ := DeviceURL(srv.URL())
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecorder()
	first := New(Options{URL: u, Credential: credentials.NewSecret("cred-x"), BackoffMin: 10 * time.Millisecond,
		ReplacedBackoff: time.Hour, Log: quiet}, rec)
	go first.Run(ctx)
	waitFrame(t, rec, protocol.TypeConfig)
	second := New(Options{URL: u, Credential: credentials.NewSecret("cred-x"), Log: quiet}, newRecorder())
	go second.Run(ctx)
	if err := <-rec.gotDown; err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("err %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := srv.Connections(); n != 2 || !second.Connected() {
		t.Fatalf("connections %d, newcomer connected %v", n, second.Connected())
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
