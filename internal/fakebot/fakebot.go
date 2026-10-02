// Package fakebot is a stand-in for OpenVibe.Bot used by tests. It speaks Bot's real device contract (docs/protocol.md
// section 1 and POST /api/v1/pair, pinned in internal/protocol/testdata/bot): it redeems pairing codes, upgrades every
// /device request and then authenticates the Authorization header (a bad credential closes with 4002, a revoked
// device with 4003, a second connection replaces the first with 4000), sends hello and config, and answers
// heartbeats. Tests drive it to send commands, e-stops and silence, and read back what the device sent.
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
	// Greeting, when set, replaces hello and config with these exact frames (fixtures).
	Greeting [][]byte
	// PairBody, when set, is the exact 201 body of a successful POST /api/v1/pair (fixtures). The credential in it is
	// registered for its device_id.
	PairBody []byte
	// WHIP, when set, serves POST/DELETE under /whip/.
	WHIP http.Handler

	mu           sync.Mutex
	codes        map[string]string // code → robot id
	creds        map[string]string // credential → device id
	robots       map[string]string // device id → robot id
	live         map[string]*Conn  // device id → its current connection
	estop        map[string]bool   // robot id → the robot's e-stop latch, as Bot keeps it
	pairRequests []protocol.PairRequest
	credInURL    bool

	conns chan *Conn
	count atomic.Int64
}

// RobotID is the robot AddCode and AddCredential attach devices to.
const RobotID = "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X"

// New starts the fake server.
func New() *Server {
	s := &Server{codes: map[string]string{}, creds: map[string]string{}, robots: map[string]string{}, live: map[string]*Conn{}}
	s.estop = map[string]bool{}
	s.conns = make(chan *Conn, 16)
	s.Config = protocol.Config{HeartbeatMS: 1000, Limits: protocol.Limits{MaxCommandMS: 1000, HeartbeatMS: 1000}}
	s.Config.AllowedCommands = append([]string(nil), protocol.Kinds...)
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

// AddCode makes a one-time pairing code (normalized, without the dash) valid for RobotID.
func (s *Server) AddCode(code string) {
	s.mu.Lock()
	s.codes[code] = RobotID
	s.mu.Unlock()
}

// AddCredential registers a credential directly (skipping pairing).
func (s *Server) AddCredential(cred, deviceID string) {
	s.mu.Lock()
	s.creds[cred] = deviceID
	s.robots[deviceID] = RobotID
	s.mu.Unlock()
}

// Revoke invalidates a credential and closes the device's live connection with 4003, as Bot's revoke does.
func (s *Server) Revoke(cred string) {
	s.mu.Lock()
	dev := s.creds[cred]
	delete(s.creds, cred)
	c := s.live[dev]
	delete(s.live, dev)
	s.mu.Unlock()
	if c != nil {
		c.CloseWith(protocol.CloseRevoked, "revoked")
	}
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

// Connections counts accepted (authenticated) device connections.
func (s *Server) Connections() int { return int(s.count.Load()) }

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// problem writes an RFC 9457 body the way Bot's API does.
func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://openvibe.network/problems/" + code,
		"title": http.StatusText(status), "status": status, "code": code, "detail": detail, "error": detail})
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		problem(w, http.StatusMethodNotAllowed, "bot.method_not_allowed", "POST only")
		return
	}
	var req protocol.PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "bot.bad_json", "the body must be JSON")
		return
	}
	code := strings.ToUpper(strings.ReplaceAll(req.Code, "-", ""))
	s.mu.Lock()
	s.pairRequests = append(s.pairRequests, req)
	robot, ok := s.codes[code]
	if ok && req.Robot != "" && req.Robot != robot {
		ok = false
	}
	if ok {
		delete(s.codes, code)
	}
	s.mu.Unlock()
	if !ok {
		problem(w, http.StatusForbidden, "bot.pairing_code_invalid", "that is not a live pairing code")
		return
	}
	body := s.PairBody
	if body == nil {
		cred, dev := token(), "dev_"+token()[:12]
		body, _ = json.Marshal(protocol.PairResponse{DeviceID: dev, Credential: cred, PublishKey: "pk_" + token()[:16],
			RobotID: robot, Profile: json.RawMessage(`{"id":"sim.rover","limits":{"max_command_ms":300,"heartbeat_ms":1000}}`)})
	}
	var pr protocol.PairResponse
	_ = json.Unmarshal(body, &pr)
	s.mu.Lock()
	s.creds[pr.Credential] = pr.DeviceID
	s.robots[pr.DeviceID] = pr.RobotID
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
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
	s.mu.Unlock()
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Conn{ws: ws, recv: make(chan protocol.Frame, 1024), done: make(chan struct{})}
	if auth == "" {
		// Bot: a socket without a credential may only pair; everything else is error bot.not_paired.
		go c.unpaired()
		return
	}
	s.mu.Lock()
	dev, ok := s.creds[strings.TrimPrefix(auth, "Bearer ")]
	if ok && strings.HasPrefix(auth, "Bearer ") {
		c.DeviceID, c.RobotID = dev, s.robots[dev]
	}
	old := s.live[dev]
	if c.DeviceID != "" {
		s.live[dev] = c
	}
	greeting := s.Greeting
	cfg := s.Config
	cfg.EstopLatched = s.estop[c.RobotID]
	c.srv = s
	s.mu.Unlock()
	if c.DeviceID == "" {
		c.CloseWith(protocol.CloseInvalid, "invalid credential")
		return
	}
	if old != nil {
		old.CloseWith(protocol.CloseReplaced, "replaced by a newer connection")
	}
	s.count.Add(1)
	c.autoAck.Store(true)
	if greeting != nil {
		for _, b := range greeting {
			_ = c.SendRaw(b)
		}
	} else {
		_ = c.Send(protocol.Hello{SessionID: "sess_" + token()[:12], DeviceID: dev, RobotIDs: []string{c.RobotID},
			ServerTime: time.Now().UTC().Format(time.RFC3339Nano)})
		_ = c.Send(cfg)
	}
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
	RobotID  string
	srv      *Server
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
func (c *Conn) Send(m protocol.Message) error { return c.sendAt(time.Now().UnixMilli(), m) }

// Command sends a command the way Bot does: deadline_ms is the absolute instant ts + deadlineMS (none when 0), and
// the operator is usr_test unless cmd names one.
func (c *Conn) Command(cmd protocol.Command, deadlineMS int) error {
	ts := time.Now().UnixMilli()
	if deadlineMS > 0 {
		cmd.DeadlineMS = ts + int64(deadlineMS)
	}
	if cmd.Operator == nil {
		cmd.Operator = &protocol.Operator{Subject: "usr_test", Role: "operator"}
	}
	if cmd.RobotID == "" {
		cmd.RobotID = c.RobotID
	}
	return c.sendAt(ts, cmd)
}

// setEstop records the robot's latch: Bot keeps it from estop frames it sends and estop_state frames it receives.
func (c *Conn) setEstop(latched bool) {
	if c.srv == nil {
		return
	}
	c.srv.mu.Lock()
	c.srv.estop[c.RobotID] = latched
	c.srv.mu.Unlock()
}

func (c *Conn) sendAt(ts int64, m protocol.Message) error {
	if e, ok := m.(protocol.Estop); ok {
		c.setEstop(e.Latched)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	b, err := protocol.Encode(c.seq, ts, m)
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// CloseWith ends the connection with a WebSocket close code, as Bot does for 4000, 4002 and 4003.
func (c *Conn) CloseWith(code int, reason string) {
	c.wmu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	c.wmu.Unlock()
	_ = c.ws.Close()
}

func (c *Conn) unpaired() {
	defer c.ws.Close()
	for {
		if _, _, err := c.ws.ReadMessage(); err != nil {
			return
		}
		_ = c.Send(protocol.Error{Code: protocol.ErrNotPaired, Detail: "send pair first"})
	}
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
		if es, ok := f.Msg.(protocol.EstopState); ok {
			c.setEstop(es.Latched)
		}
		if hb, ok := f.Msg.(protocol.Heartbeat); ok && c.autoAck.Load() && !c.muted.Load() {
			_ = c.Send(protocol.HeartbeatAck{Seq: hb.Seq, ServerTime: time.Now().UTC().Format(time.RFC3339Nano)})
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
