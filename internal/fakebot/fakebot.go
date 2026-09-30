// Package fakebot is a stand-in for OpenVibe.Bot used by tests: it redeems pairing codes, accepts device WebSockets
// (credential in the Authorization header only), speaks the ADR-043 message set, and can mount a WHIP endpoint. Tests
// drive it to send commands, e-stops and silence, and read back what the device sent.
package fakebot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Server is the fake service.
type Server struct {
	HTTP *httptest.Server

	// Config is sent to every device right after hello.
	Config protocol.Config
	// WHIP, when set, serves POST/DELETE under /whip/.
	WHIP http.Handler

	mu           sync.Mutex
	codes        map[string]bool
	creds        map[string]string // credential → device id
	pairRequests []protocol.PairRequest
	credInURL    bool

	conns chan *Conn
	count atomic.Int64
}

// New starts the fake server.
func New() *Server {
	s := &Server{
		codes: map[string]bool{}, creds: map[string]string{}, conns: make(chan *Conn, 16),
		Config: protocol.Config{HeartbeatMS: 1000, Limits: protocol.Limits{MaxCommandMS: 1000}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PairPath, s.pair)
	mux.HandleFunc(protocol.DevicePath, s.device)
	mux.HandleFunc("/whip/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h := s.WHIP
		s.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	s.HTTP = httptest.NewServer(mux)
	return s
}

func (s *Server) Close() { s.HTTP.Close() }

// URL is the origin, e.g. http://127.0.0.1:1234.
func (s *Server) URL() string { return s.HTTP.URL }

// SetWHIP mounts a WHIP handler.
func (s *Server) SetWHIP(h http.Handler) {
	s.mu.Lock()
	s.WHIP = h
	s.mu.Unlock()
}

// AddCode makes a one-time pairing code valid.
func (s *Server) AddCode(code string) {
	s.mu.Lock()
	s.codes[code] = true
	s.mu.Unlock()
}

// AddCredential registers a credential directly (skipping pairing).
func (s *Server) AddCredential(cred, deviceID string) {
	s.mu.Lock()
	s.creds[cred] = deviceID
	s.mu.Unlock()
}

// Revoke invalidates a credential for future connections.
func (s *Server) Revoke(cred string) {
	s.mu.Lock()
	delete(s.creds, cred)
	s.mu.Unlock()
}

// PairRequests returns what devices sent to /api/v1/pair.
func (s *Server) PairRequests() []protocol.PairRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.PairRequest(nil), s.pairRequests...)
}

// CredentialSeenInURL reports whether any request carried the credential in its URL.
func (s *Server) CredentialSeenInURL() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credInURL
}

// Connections counts accepted device connections.
func (s *Server) Connections() int { return int(s.count.Load()) }

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.pairRequests = append(s.pairRequests, req)
	ok := s.codes[req.Code]
	delete(s.codes, req.Code)
	var cred, dev string
	if ok {
		cred, dev = token(), "dev_"+token()[:12]
		s.creds[cred] = dev
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(protocol.PairError{Error: "invalid_code"})
		return
	}
	_ = json.NewEncoder(w).Encode(protocol.PairResponse{
		DeviceID: dev, Credential: cred, PublishKey: "pk_" + token()[:16],
		WHIPURL: s.HTTP.URL + "/whip/" + dev, Profile: json.RawMessage(`{"profile":"sim.rover","capabilities":["drive"]}`),
	})
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	for cred := range s.creds {
		if strings.Contains(r.URL.RawQuery, cred) || strings.Contains(r.URL.Path, cred) {
			s.credInURL = true
		}
	}
	auth := r.Header.Get("Authorization")
	dev, ok := s.creds[strings.TrimPrefix(auth, "Bearer ")]
	s.mu.Unlock()
	if !strings.HasPrefix(auth, "Bearer ") || !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.count.Add(1)
	c := &Conn{ws: ws, DeviceID: dev, recv: make(chan protocol.Frame, 1024), done: make(chan struct{})}
	c.autoAck.Store(true)
	_ = c.Send(protocol.Hello{DeviceID: dev, Session: token()[:8], ServerTime: time.Now().UnixMilli()})
	_ = c.Send(s.Config)
	go c.read()
	select {
	case s.conns <- c:
	default:
	}
}

// NextConn waits for the next device connection.
func (s *Server) NextConn(timeout time.Duration) (*Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("no device connected within %s", timeout)
	}
}

// Conn is one device connection as the server sees it.
type Conn struct {
	DeviceID string
	ws       *websocket.Conn
	wmu      sync.Mutex
	seq      uint64
	recv     chan protocol.Frame
	done     chan struct{}
	autoAck  atomic.Bool
	muted    atomic.Bool
	frames   []protocol.Frame
	fmu      sync.Mutex
}

// Send writes one frame to the device.
func (c *Conn) Send(m protocol.Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	b, err := protocol.Encode(c.seq, time.Now().UnixMilli(), m)
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// SendRaw writes arbitrary bytes (for malformed-frame tests).
func (c *Conn) SendRaw(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// Mute stops answering heartbeats and sending anything, as a hung server would.
func (c *Conn) Mute() { c.muted.Store(true); c.autoAck.Store(false) }

// Close drops the connection.
func (c *Conn) Close() { _ = c.ws.Close() }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) read() {
	defer close(c.done)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		f, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		if hb, ok := f.Msg.(protocol.Heartbeat); ok && c.autoAck.Load() && !c.muted.Load() {
			_ = c.Send(protocol.HeartbeatAck{T: hb.T})
		}
		c.fmu.Lock()
		c.frames = append(c.frames, f)
		c.fmu.Unlock()
		select {
		case c.recv <- f:
		default:
		}
	}
}

// Frames returns everything received so far.
func (c *Conn) Frames() []protocol.Frame {
	c.fmu.Lock()
	defer c.fmu.Unlock()
	return append([]protocol.Frame(nil), c.frames...)
}

// Expect waits for the next frame of the given type whose message satisfies match (nil matches all), skipping others.
func (c *Conn) Expect(typ string, timeout time.Duration, match func(protocol.Message) bool) (protocol.Frame, error) {
	t := time.After(timeout)
	for {
		select {
		case f := <-c.recv:
			if f.Type == typ && (match == nil || match(f.Msg)) {
				return f, nil
			}
		case <-t:
			return protocol.Frame{}, fmt.Errorf("no %s frame within %s", typ, timeout)
		case <-c.done:
			return protocol.Frame{}, fmt.Errorf("connection closed while waiting for %s", typ)
		}
	}
}

// Reply waits for the ack or nack of command id.
func (c *Conn) Reply(id string, timeout time.Duration) (protocol.Message, error) {
	t := time.After(timeout)
	for {
		select {
		case f := <-c.recv:
			switch m := f.Msg.(type) {
			case protocol.Ack:
				if m.ID == id {
					return m, nil
				}
			case protocol.Nack:
				if m.ID == id {
					return m, nil
				}
			}
		case <-t:
			return nil, fmt.Errorf("no reply to %s within %s", id, timeout)
		case <-c.done:
			return nil, fmt.Errorf("connection closed while waiting for the reply to %s", id)
		}
	}
}
