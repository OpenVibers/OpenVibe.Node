// Package link keeps the Node's one outbound control connection to OpenVibe.Bot: a WebSocket to wss://<origin>/device
// authenticated with the device credential in the Authorization header of the upgrade (never in the URL). It sends a
// heartbeat every interval, declares the link lost when nothing has arrived for the deadman interval (two heartbeats
// by default), and reconnects with exponential backoff and full jitter. Nothing is queued while offline: Send fails
// and the caller decides. The connection counts as up only once the server's hello arrives: OpenVibe.Bot
// authenticates after the upgrade and answers anything sent earlier with error bot.not_paired.
//
// Close codes: 4000 (another connection with this credential replaced this one) waits ReplacedBackoff before trying
// again, so two agents sharing a credential do not take turns kicking each other off; 4003 (revoked) ends Run;
// 4002 (credential refused) ends Run after MaxRefused refusals in a row, so a server blip is survived.
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
	// Connected runs when the server's hello arrives, before the hello itself reaches Frame. The handler sends status
	// and estop_state here.
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
	ReplacedBackoff   time.Duration // wait after close 4000; default 60 s
	MaxRefused        int           // consecutive 4002 / HTTP 401 before giving up; default 3
}

// Link is the reconnecting control connection.
type Link struct {
	opt     Options
	handler Handler
	log     *slog.Logger

	mu        sync.Mutex
	out       chan []byte
	seq       uint64
	hbSent    map[uint64]time.Time // heartbeat seq → send time, for the RTT
	heartbeat time.Duration
	deadman   time.Duration

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
	if opt.ReplacedBackoff <= 0 {
		opt.ReplacedBackoff = 60 * time.Second
	}
	if opt.MaxRefused <= 0 {
		opt.MaxRefused = 3
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
		// The body seq equals the envelope seq, so heartbeat_ack's echo is the same whichever the server reads.
		hb.Seq = l.seq
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
		l.hbSent[l.seq] = now
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

// Run connects and reconnects until ctx ends, the credential is revoked (close 4003), or the server has refused the
// credential MaxRefused times in a row.
func (l *Link) Run(ctx context.Context) {
	attempt, refused := 0, 0
	for ctx.Err() == nil {
		start := time.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := errMsg(err)
		l.lastErr.Store(msg)
		if errors.Is(err, errRevoked) {
			l.log.Error("link stopped", "err", msg)
			return
		}
		if errors.Is(err, errUnauthorized) {
			if refused++; refused >= l.opt.MaxRefused {
				l.log.Error("link stopped: the server refused the credential", "times", refused, "err", msg)
				return
			}
		} else {
			refused = 0
		}
		if time.Since(start) > 30*time.Second {
			attempt = 0
		}
		max := l.opt.BackoffMin << attempt
		if max > l.opt.BackoffMax || max <= 0 {
			max = l.opt.BackoffMax
		}
		if errors.Is(err, errUnauthorized) && max < 10*time.Second {
			max = 10 * time.Second
		}
		// Full jitter, with a floor so a flapping server is not hammered.
		wait := l.opt.BackoffMin/2 + time.Duration(rand.Int63n(int64(max)))
		if errors.Is(err, errReplaced) {
			// Another agent holds this credential now. Fighting it would flap both; come back much later.
			wait = l.opt.ReplacedBackoff + time.Duration(rand.Int63n(int64(l.opt.ReplacedBackoff)/4+1))
		}
		attempt++
		l.reconnects.Add(1)
		l.log.Warn("link down; reconnecting", "err", msg, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

var (
	errUnauthorized = errors.New("the server refused the device credential (revoked or rotated?); pair again with `openvibe-node pair <CODE>`")
	errRevoked      = errors.New("the owner revoked this device; pair again with `openvibe-node pair <CODE>`")
	errReplaced     = errors.New("another connection with this device's credential replaced this one (is a second agent running with the same credential?)")
)

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
	l.out, l.seq, l.hbSent = nil, 0, map[uint64]time.Time{}
	l.mu.Unlock()

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

	up := false
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
		lastRx.Store(time.Now().UnixNano())
		f, err := protocol.Decode(data)
		if err != nil {
			if errors.Is(err, protocol.ErrUnknownType) {
				l.log.Debug("ignoring unknown frame", "type", f.Type)
			} else {
				l.log.Warn("bad frame", "err", err)
			}
			continue
		}
		switch m := f.Msg.(type) {
		case protocol.HeartbeatAck:
			l.mu.Lock()
			sent, ok := l.hbSent[m.Seq]
			delete(l.hbSent, m.Seq)
			l.mu.Unlock()
			if ok {
				l.lastRTT.Store(int64(time.Since(sent)))
			}
			continue
		case protocol.Error:
			if m.Code == protocol.ErrNotPaired {
				l.log.Warn("server: this connection is not authenticated yet", "code", m.Code, "detail", m.Detail)
			} else {
				l.log.Warn("server refused a frame", "code", m.Code, "detail", m.Detail)
			}
			continue
		case protocol.Hello:
			if !up {
				up = true
				l.mu.Lock()
				l.out = out
				l.mu.Unlock()
				l.connected.Store(true)
				l.connectedAt.Store(time.Now().UnixMilli())
				l.log.Info("link up", "url", redactURL(l.opt.URL), "device", m.DeviceID, "session", m.SessionID)
				l.handler.Connected()
			}
		}
		if !up {
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
