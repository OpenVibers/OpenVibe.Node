// Package link keeps the Node's one outbound control connection to OpenVibe.Bot: a WebSocket to wss://<origin>/device
// authenticated with the device credential in the Authorization header of the upgrade (never in the URL). It sends a
// heartbeat every interval once the server has spoken, measures the round trip from the server's heartbeat_ack and
// reports it on the next heartbeat, declares the link lost when nothing has arrived for the deadman interval (two
// heartbeats by default), and reconnects with exponential backoff and full jitter. A refused or revoked credential
// (close 4002/4003) waits at least CredentialBackoff (10 s) before the next try. Nothing is queued while offline: Send
// fails and the caller decides.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenVibers/OpenVibe.Node/internal/credentials"
	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// ErrOffline is returned by Send while there is no connection.
var ErrOffline = errors.New("link offline")

// Handler receives link events. Calls are made from the link's goroutines, one at a time per connection.
type Handler interface {
	// Connected runs after the upgrade, before any frame is read. The server may still be checking the credential, so
	// the handler sends nothing here: status and estop_state wait for hello and config.
	Connected()
	// Frame is every decoded server frame except heartbeat_ack.
	Frame(f protocol.Frame)
	// Disconnected runs when a connection ends for any reason. The handler must stop every actuator.
	Disconnected(err error)
}

// Options configure a Link.
type Options struct {
	URL        string // wss://openvibe.bot/device
	Credential credentials.Secret
	UserAgent  string
	Log        *slog.Logger
	Dialer     *websocket.Dialer

	HeartbeatInterval time.Duration // default 1 s
	Deadman           time.Duration // default 2 × heartbeat
	BackoffMin        time.Duration // default 500 ms
	BackoffMax        time.Duration // default 30 s
	CredentialBackoff time.Duration // default 10 s: the least wait after a refused credential; jitter goes on top
}

// helloTimeout is how long a new connection waits for the server's first frame (hello) before giving up; the deadman
// applies once the server has spoken.
const helloTimeout = 10 * time.Second

// Link is the reconnecting control connection.
type Link struct {
	opt     Options
	handler Handler
	log     *slog.Logger

	mu        sync.Mutex
	out       chan []byte
	seq       uint64
	heartbeat time.Duration
	deadman   time.Duration
	hbSent    map[uint64]time.Time // heartbeat seq → when it was sent, until its ack arrives

	connected   atomic.Bool
	lastRTT     atomic.Int64
	reconnects  atomic.Int64
	lastErr     atomic.Value // string
	connectedAt atomic.Int64
}

func New(opt Options, h Handler) *Link {
	if opt.HeartbeatInterval <= 0 {
		opt.HeartbeatInterval = time.Duration(protocol.DefaultHeartbeatMS) * time.Millisecond
	}
	if opt.Deadman <= 0 {
		opt.Deadman = 2 * opt.HeartbeatInterval
	}
	if opt.BackoffMin <= 0 {
		opt.BackoffMin = 500 * time.Millisecond
	}
	if opt.BackoffMax <= 0 {
		opt.BackoffMax = 30 * time.Second
	}
	if opt.CredentialBackoff <= 0 {
		opt.CredentialBackoff = 10 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Dialer == nil {
		opt.Dialer = &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 15 * time.Second}
	}
	l := &Link{opt: opt, handler: h, log: opt.Log.With("component", "link"), heartbeat: opt.HeartbeatInterval, deadman: opt.Deadman}
	l.lastErr.Store("")
	return l
}

// SetTiming applies the server's heartbeat_ms (from config) to the current and later connections; the deadman is two
// heartbeats.
func (l *Link) SetTiming(heartbeatMS int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if heartbeatMS > 0 {
		l.heartbeat = time.Duration(heartbeatMS) * time.Millisecond
		l.deadman = 2 * l.heartbeat
	}
}

func (l *Link) timing() (time.Duration, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heartbeat, l.deadman
}

// Connected reports whether a connection is up.
func (l *Link) Connected() bool { return l.connected.Load() }

// Stats for status.
type Stats struct {
	Connected  bool
	Since      time.Time
	RTT        time.Duration
	Reconnects int64
	LastError  string
}

func (l *Link) Stats() Stats {
	s := Stats{Connected: l.connected.Load(), RTT: time.Duration(l.lastRTT.Load()), Reconnects: l.reconnects.Load(),
		LastError: l.lastErr.Load().(string)}
	if at := l.connectedAt.Load(); at > 0 && s.Connected {
		s.Since = time.UnixMilli(at)
	}
	return s
}

// Send queues a frame on the current connection. It never waits for a connection: offline means ErrOffline.
func (l *Link) Send(m protocol.Message) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.sendLocked(m)
	return err
}

func (l *Link) sendLocked(m protocol.Message) (uint64, error) {
	if l.out == nil {
		return 0, ErrOffline
	}
	l.seq++
	b, err := protocol.Encode(l.seq, time.Now().UnixMilli(), m)
	if err != nil {
		return 0, err
	}
	select {
	case l.out <- b:
		return l.seq, nil
	default:
		return 0, errors.New("link send queue full")
	}
}

// sendHeartbeat sends a heartbeat carrying the last measured round trip and remembers when it left.
func (l *Link) sendHeartbeat(now time.Time) {
	var hb protocol.Heartbeat
	if rtt := l.lastRTT.Load(); rtt > 0 {
		ms := time.Duration(rtt).Milliseconds()
		hb.RTTMS = &ms
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seq, err := l.sendLocked(hb)
	if err != nil {
		return
	}
	if len(l.hbSent) >= 32 { // acks that never came
		l.hbSent = map[uint64]time.Time{}
	}
	l.hbSent[seq] = now
}

// heartbeatAck measures the round trip of the heartbeat the ack answers: the echo field when the server sends one,
// else the envelope seq, which Bot overwrites with the heartbeat's seq.
func (l *Link) heartbeatAck(f protocol.Frame, ack protocol.HeartbeatAck) {
	seq := f.Seq
	if ack.Echo != nil {
		seq = *ack.Echo
	}
	l.mu.Lock()
	sent, ok := l.hbSent[seq]
	for s := range l.hbSent {
		if s <= seq {
			delete(l.hbSent, s)
		}
	}
	l.mu.Unlock()
	if ok {
		rtt := time.Since(sent)
		if rtt < time.Millisecond {
			rtt = time.Millisecond
		}
		l.lastRTT.Store(int64(rtt))
	}
}

// Run connects and reconnects until ctx ends.
func (l *Link) Run(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		start := time.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := errMsg(err)
		l.lastErr.Store(msg)
		if time.Since(start) > 30*time.Second {
			attempt = 0
		}
		wait := l.backoff(err, attempt)
		attempt++
		l.reconnects.Add(1)
		if IsCredentialError(err) {
			l.log.Error("the server refused the device credential: re-pair or update the credential", "err", msg,
				"retry_in", wait.Round(time.Millisecond))
		} else {
			l.log.Warn("link down; reconnecting", "err", msg, "in", wait.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// backoff is the wait before reconnect attempt n: full jitter over an exponential window, with a floor so a flapping
// server is not hammered. A refused credential waits at least CredentialBackoff, the jitter on top.
func (l *Link) backoff(err error, attempt int) time.Duration {
	window := l.opt.BackoffMin
	if attempt < 30 {
		window = l.opt.BackoffMin << attempt
	}
	if window > l.opt.BackoffMax || window <= 0 {
		window = l.opt.BackoffMax
	}
	jitter := time.Duration(rand.Int63n(int64(window)))
	if IsCredentialError(err) {
		return l.opt.CredentialBackoff + jitter
	}
	return l.opt.BackoffMin/2 + jitter
}

// CredentialError is a refused credential: close 4002 (unknown, or a rotated one past its grace), close 4003
// (revoked), or HTTP 401/403 on the upgrade.
type CredentialError struct {
	Code   int // close code or HTTP status
	Reason string
}

func (e *CredentialError) Error() string {
	what := "the server refused the device credential"
	if e.Code == protocol.CloseRevoked {
		what = "the owner revoked this device"
	}
	return fmt.Sprintf("%s (%d %s); re-pair or update the credential: `openvibe-node pair --force <CODE>`, or "+
		"`openvibe-node credential set` with a rotated one", what, e.Code, e.Reason)
}

// IsCredentialError reports whether err means the credential must be replaced.
func IsCredentialError(err error) bool {
	var ce *CredentialError
	return errors.As(err, &ce)
}

// closeError classifies the server's close frame.
func closeError(ce *websocket.CloseError) error {
	switch ce.Code {
	case protocol.CloseBadCredential, protocol.CloseRevoked:
		return &CredentialError{Code: ce.Code, Reason: ce.Text}
	case protocol.CloseReplaced:
		return fmt.Errorf("replaced by a newer connection of this device (close %d)", ce.Code)
	}
	return fmt.Errorf("closed by the server (%d %s)", ce.Code, ce.Text)
}

func errMsg(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}

func (l *Link) session(ctx context.Context) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+l.opt.Credential.Reveal())
	if l.opt.UserAgent != "" {
		h.Set("User-Agent", l.opt.UserAgent)
	}
	conn, resp, err := l.opt.Dialer.DialContext(ctx, l.opt.URL, h)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return &CredentialError{Code: resp.StatusCode, Reason: http.StatusText(resp.StatusCode)}
		}
		if resp != nil {
			return fmt.Errorf("connect %s: HTTP %d", redactURL(l.opt.URL), resp.StatusCode)
		}
		return fmt.Errorf("connect %s: %s", redactURL(l.opt.URL), scrub(err.Error(), l.opt.Credential))
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	out := make(chan []byte, 256)
	l.mu.Lock()
	l.out, l.seq, l.hbSent = out, 0, map[uint64]time.Time{}
	l.mu.Unlock()
	l.connected.Store(true)
	l.connectedAt.Store(time.Now().UnixMilli())
	l.log.Info("link up", "url", redactURL(l.opt.URL))

	sessCtx, cancel := context.WithCancel(ctx)
	var endErr error
	var endOnce sync.Once
	end := func(err error) {
		endOnce.Do(func() {
			endErr = err
			cancel()
			_ = conn.Close()
		})
	}
	defer func() {
		l.mu.Lock()
		l.out = nil
		l.mu.Unlock()
		l.connected.Store(false)
		end(nil)
		l.handler.Disconnected(endErr)
	}()

	// Ending the session (or the Node) closes the socket, which ends the read loop below.
	go func() {
		<-sessCtx.Done()
		end(ctx.Err())
	}()

	var lastRx atomic.Int64
	lastRx.Store(time.Now().UnixNano())
	// Heartbeats start once the server has spoken (hello): until then it may still be checking the credential.
	var heard atomic.Bool

	// Writer.
	go func() {
		for {
			select {
			case <-sessCtx.Done():
				return
			case b := <-out:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
					end(fmt.Errorf("write: %w", err))
					return
				}
			}
		}
	}()

	// Heartbeat sender and deadman watchdog.
	go func() {
		hb, _ := l.timing()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		next := time.Now()
		for {
			select {
			case <-sessCtx.Done():
				return
			case now := <-tick.C:
				hbNow, deadman := l.timing()
				if hbNow != hb {
					hb, next = hbNow, now
				}
				if heard.Load() && !now.Before(next) {
					next = now.Add(hb)
					l.sendHeartbeat(now)
				}
				limit := deadman
				if !heard.Load() && limit < helloTimeout {
					limit = helloTimeout // the server is still checking the credential
				}
				if now.Sub(time.Unix(0, lastRx.Load())) > limit {
					end(fmt.Errorf("heartbeat lost: nothing from the server for %s", limit))
					return
				}
			}
		}
	}()

	l.handler.Connected()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if sessCtx.Err() == nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					end(closeError(ce))
				} else {
					end(fmt.Errorf("read: %s", scrub(err.Error(), l.opt.Credential)))
				}
			}
			return endErr
		}
		lastRx.Store(time.Now().UnixNano())
		heard.Store(true)
		f, err := protocol.Decode(data)
		if err != nil {
			if errors.Is(err, protocol.ErrUnknownType) {
				l.log.Debug("ignoring unknown frame", "type", f.Type)
			} else {
				l.log.Warn("bad frame", "err", err)
			}
			continue
		}
		if ack, ok := f.Msg.(protocol.HeartbeatAck); ok {
			l.heartbeatAck(f, ack)
			continue
		}
		l.handler.Frame(f)
	}
}

// DeviceURL derives wss://host/device from the server origin.
func DeviceURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("server %q must be https://", server)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + protocol.DevicePath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(invalid url)"
	}
	u.User, u.RawQuery = nil, ""
	return u.String()
}

func scrub(s string, secret credentials.Secret) string {
	if v := secret.Reveal(); v != "" {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}
