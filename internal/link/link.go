// Package link keeps the Node's one outbound control connection to OpenVibe.Bot: a WebSocket to wss://<origin>/device
// authenticated with the device credential in the Authorization header of the upgrade (never in the URL). It sends a
// heartbeat every interval from the server's hello on, declares the link lost when nothing has arrived for the deadman
// interval (two heartbeats by default; HelloAllowance before hello), and reconnects with exponential backoff and full
// jitter. Nothing is queued while offline: Send fails and the caller decides. The connection counts as up only once
// the server's hello arrives: OpenVibe.Bot authenticates after the upgrade and answers anything sent earlier with
// error bot.not_paired.
//
// Close codes: 4000 (another connection with this credential replaced this one) reconnects with the normal backoff;
// 4002 (credential refused) and 4003 (revoked) keep retrying, never sooner than CredentialRetryMin, and log what to
// do: pair again, or import the owner's rotation. Bot's error frames are logged, never fatal.
//
// Every server frame's ts also feeds an estimate of the server's clock (ServerNow), which the Node uses to read the
// absolute deadline_ms of a command.
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
	// Connected runs when the server's hello arrives, before the hello itself reaches Frame. Bot sends config right
	// after hello; the Node holds its first status and estop_state (and every motion command) until that config.
	Connected()
	// Frame is every decoded server frame except heartbeat_ack and error.
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
}

// CredentialRetryMin is the least a refused or revoked credential (close 4002 / 4003, HTTP 401 / 403) waits before
// the next try; jitter only ever adds to it.
const CredentialRetryMin = 10 * time.Second

// HelloAllowance is how long a new connection may stay silent before the server's hello when the deadman is shorter:
// Bot authenticates after the upgrade, and heartbeats (and so the deadman) only start once hello has arrived.
const HelloAllowance = 10 * time.Second

// hbSend is one heartbeat the link sent: its monotonic send time and the t it carried, so an ack can be matched
// back either by the echoed t (Bot) or by the body seq (older servers).
type hbSend struct {
	at time.Time
	t  int64
}

// Link is the reconnecting control connection.
type Link struct {
	opt     Options
	handler Handler
	log     *slog.Logger

	mu        sync.Mutex
	out       chan []byte
	seq       uint64
	hbSent    map[uint64]hbSend // heartbeat seq → send time/t, bounded to the last few
	heartbeat time.Duration
	deadman   time.Duration

	// The server clock estimate of this connection (see ServerNow): skew is the largest ts − local receive time (ms)
	// seen since clockBase, the local time of its first frame; hasSkew is false until a frame arrives.
	skew      int64
	hasSkew   bool
	clockBase time.Time

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

// SetTiming applies the server's config.heartbeat_ms to the current and later connections; the link is declared
// lost after two intervals without a frame.
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

// observe records one server frame's ts, read at local time at.
func (l *Link) observe(ts int64, at time.Time) {
	if ts <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.hasSkew {
		l.clockBase, l.skew, l.hasSkew = at, ts-at.UnixMilli(), true
		return
	}
	l.skew = max(l.skew, ts-l.localMS(at))
}

// localMS is local time at in epoch ms, measured from clockBase on the monotonic clock so a step of the wall clock
// cannot move the estimate.
func (l *Link) localMS(at time.Time) int64 {
	return l.clockBase.UnixMilli() + at.Sub(l.clockBase).Milliseconds()
}

// ServerNow estimates the server's clock (epoch ms) at local time at, from the frames of the current connection. The
// offset is the largest ts − receive time seen on the connection: the clock skew less the fastest transit, so a frame
// that sat in a queue on the way reads as late by however long it sat there. The best sample is kept for the whole
// connection: a run of delayed frames can never make an expired deadline look open again. A server clock that steps
// back only makes deadlines read as expired (fail closed) until the next connection. ok is false before any frame.
func (l *Link) ServerNow(at time.Time) (ms int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.hasSkew {
		return 0, false
	}
	return l.localMS(at) + l.skew, true
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
	if l.out == nil {
		return ErrOffline
	}
	l.seq++
	now := time.Now()
	if hb, ok := m.(protocol.Heartbeat); ok {
		// The body seq still mirrors the envelope's (older servers echo it); t is the send time Bot echoes back as
		// heartbeat_ack.t/echo, and rtt_ms is the last measured round trip, once there is one on this connection.
		hb.Seq = l.seq
		hb.T = now.UnixMilli()
		if rtt := l.lastRTT.Load(); rtt > 0 {
			ms := time.Duration(rtt).Milliseconds()
			hb.RTTMS = &ms
		}
		m = hb
		for s := range l.hbSent {
			if s+8 < l.seq {
				delete(l.hbSent, s)
			}
		}
		l.hbSent[l.seq] = hbSend{at: now, t: hb.T}
	}
	b, err := protocol.Encode(l.seq, now.UnixMilli(), m)
	if err != nil {
		return err
	}
	select {
	case l.out <- b:
		return nil
	default:
		return errors.New("link send queue full")
	}
}

// Run connects and reconnects until ctx ends. A refused or revoked credential does not end it: the owner may pair
// the device again, and the Node retries every CredentialRetryMin or more, saying so in the log.
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
		if credentialRefused(err) {
			l.log.Error("link down: the server refused the device credential", "err", msg, "retry_in", wait.Round(time.Millisecond))
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

// backoff is the wait before reconnect attempt number attempt (0 first) after err: exponential with full jitter and
// a floor so a flapping server is not hammered. A refused or revoked credential waits CredentialRetryMin plus the
// jitter, never less.
func (l *Link) backoff(err error, attempt int) time.Duration {
	ceil := l.opt.BackoffMax
	if attempt < 32 {
		if c := l.opt.BackoffMin << attempt; c > 0 && c < ceil {
			ceil = c
		}
	}
	jitter := time.Duration(rand.Int63n(int64(ceil) + 1))
	if credentialRefused(err) {
		return CredentialRetryMin + jitter
	}
	return l.opt.BackoffMin/2 + jitter
}

var (
	errUnauthorized = errors.New("the server refused the device credential (wrong, rotated or revoked): pair again with `openvibe-node pair <CODE>`, or, if the owner rotated it, pipe the rotate response into `openvibe-node credential import`")
	errRevoked      = errors.New("the owner revoked this device or rotated its credential: pair again with `openvibe-node pair <CODE>`, or, if the owner rotated it, pipe the rotate response into `openvibe-node credential import`")
	errReplaced     = errors.New("another connection with this device's credential replaced this one (is a second agent running with the same credential?)")
)

func credentialRefused(err error) bool {
	return errors.Is(err, errUnauthorized) || errors.Is(err, errRevoked)
}

// closeErr maps the server's close codes to the errors Run acts on.
func closeErr(err error) error {
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		return nil
	}
	switch ce.Code {
	case protocol.CloseReplaced:
		return errReplaced
	case protocol.CloseInvalid:
		return errUnauthorized
	case protocol.CloseRevoked:
		return errRevoked
	}
	return nil
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
			return errUnauthorized
		}
		if resp != nil {
			return fmt.Errorf("connect %s: HTTP %d", redactURL(l.opt.URL), resp.StatusCode)
		}
		return fmt.Errorf("connect %s: %s", redactURL(l.opt.URL), scrub(err.Error(), l.opt.Credential))
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	// Nothing is sent until the server's hello: until then the socket is not authenticated.
	out := make(chan []byte, 256)
	l.mu.Lock()
	l.out, l.seq, l.hbSent, l.hasSkew = nil, 0, map[uint64]hbSend{}, false
	l.mu.Unlock()
	// A fresh connection has no measured round trip: the first heartbeat must not claim one.
	l.lastRTT.Store(0)

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

	var up atomic.Bool // the server's hello has arrived
	var lastRx atomic.Int64
	lastRx.Store(time.Now().UnixNano())

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
				if !up.Load() {
					if now.Sub(time.Unix(0, lastRx.Load())) > max(deadman, HelloAllowance) {
						end(fmt.Errorf("no hello from the server within %s", max(deadman, HelloAllowance)))
						return
					}
					continue
				}
				if !now.Before(next) {
					next = now.Add(hb)
					_ = l.Send(protocol.Heartbeat{})
				}
				if now.Sub(time.Unix(0, lastRx.Load())) > deadman {
					end(fmt.Errorf("heartbeat lost: nothing from the server for %s", deadman))
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if sessCtx.Err() == nil {
				if ce := closeErr(err); ce != nil {
					end(ce)
				} else {
					end(fmt.Errorf("read: %s", scrub(err.Error(), l.opt.Credential)))
				}
			}
			return endErr
		}
		rx := time.Now()
		lastRx.Store(rx.UnixNano())
		f, err := protocol.Decode(data)
		if err != nil {
			if errors.Is(err, protocol.ErrUnknownType) {
				l.log.Debug("ignoring unknown frame", "type", f.Type)
			} else {
				l.log.Warn("bad frame", "err", err)
			}
			continue
		}
		l.observe(f.TS, rx)
		switch m := f.Msg.(type) {
		case protocol.HeartbeatAck:
			// Bot's envelope seq is its own counter, so the ack is matched to a stored send time by the echoed t
			// (echo, or older Bot's t); a server that only echoes the heartbeat's seq is matched by that. The
			// stored time is monotonic, so a wall-clock adjustment between send and ack cannot skew the round trip.
			echo := m.T
			if echo == 0 && m.Echo != nil {
				echo = *m.Echo
			}
			l.mu.Lock()
			var sent time.Time
			if echo > 0 {
				for s, hb := range l.hbSent {
					if hb.t == echo {
						sent = hb.at
						delete(l.hbSent, s)
						break
					}
				}
			}
			if sent.IsZero() && m.Seq != 0 {
				if hb, ok := l.hbSent[m.Seq]; ok {
					sent = hb.at
					delete(l.hbSent, m.Seq)
				}
			}
			l.mu.Unlock()
			if !sent.IsZero() {
				// A coarse clock (Windows) can read 0 on a fast loopback; 0 means "not measured yet", so floor at 1 ns.
				l.lastRTT.Store(int64(max(time.Since(sent), 1)))
			}
			continue
		case protocol.Error:
			switch m.Code {
			case protocol.ErrNotPaired:
				l.log.Warn("server: this connection is not authenticated yet", "code", m.Code, "detail", m.Detail)
			case protocol.ErrNotReady:
				l.log.Warn("server: too many frames before authentication finished", "code", m.Code, "detail", m.Detail)
			default:
				l.log.Warn("server refused a frame", "code", m.Code, "detail", m.Detail)
			}
			continue
		case protocol.Hello:
			if !up.Load() {
				up.Store(true)
				l.mu.Lock()
				l.out = out
				l.mu.Unlock()
				l.connected.Store(true)
				l.connectedAt.Store(time.Now().UnixMilli())
				l.log.Info("link up", "url", redactURL(l.opt.URL), "device", m.DeviceID, "session", m.SessionID)
				l.handler.Connected()
			}
		}
		if !up.Load() {
			l.log.Warn("ignoring a frame before hello", "type", f.Type)
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
