// Package fakebot is a stand-in for OpenVibe.Bot used by tests. It speaks Bot's real device contract (docs/protocol.md
// section 1, server/realtime.js and POST /api/v1/pair in server/api/v1.js, pinned in internal/protocol/testdata/bot):
// it redeems pairing codes (refusals are RFC 9457 problem bodies with Bot's codes), upgrades every /device request
// and then authenticates the Authorization header (a bad credential closes with 4002, a revoked device with 4003, a
// second connection replaces the first with 4000), sends hello and then config (always with allowed_commands and
// the robot's estop_latched), sends commands with a server-minted id, an absolute deadline_ms, the operator and the
// robot, and answers heartbeats. Like Bot, it keeps the robot's e-stop latch: an estop frame it sends sets or clears
// it, a device's estop_state latched:true sets it, and latched:false is only a report. Tests drive it to send
// commands, e-stops, errors and silence, and read back what the device sent.
//
// heartbeat_ack carries Bot's own envelope seq and echoes the heartbeat's t as both t and echo (echo null when the
// heartbeat had no t), with server_time as Bot's clock; the device measures RTT from the echoed send time.
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
	pairRefusal  *refusal
	bindRefusals []refusal
	credInURL    bool
	binds        atomic.Int64

	conns chan *Conn
	count atomic.Int64
	dials atomic.Int64
}

type refusal struct {
	status       int
	code, detail string
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
	mux.HandleFunc(protocol.BindPath, s.bind)
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

// RefusePair makes the next POST /api/v1/pair answer with this problem, as Bot's pairing.redeem refuses a code
// (403 bot.pairing_code_invalid / _locked / _used / _expired, 404 bot.no_pairing_code, 422 bot.invalid_pairing_code,
// 429 rate_limited).
func (s *Server) RefusePair(status int, code, detail string) {
	s.mu.Lock()
	s.pairRefusal = &refusal{status, code, detail}
	s.mu.Unlock()
}

// SetEstop sets or clears the robot's latch as the owner's REST call does: devices learn it from the next config
// (connected devices get no frame here; send protocol.Estop for that).
func (s *Server) SetEstop(latched bool) {
	s.mu.Lock()
	s.estop[RobotID] = latched
	s.mu.Unlock()
}

// EstopLatched reports the robot's latch as Bot would hold it.
func (s *Server) EstopLatched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estop[RobotID]
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

// Dials counts every /device request, authenticated or not.
func (s *Server) Dials() int { return int(s.dials.Load()) }

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
	if rf := s.pairRefusal; rf != nil {
		s.pairRefusal = nil
		s.mu.Unlock()
		problem(w, rf.status, rf.code, rf.detail)
		return
	}
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
		cred, dev, pk := token(), "dev_"+token()[:12], "pk_"+token()[:16]
		body, _ = json.Marshal(protocol.PairResponse{DeviceID: dev, Credential: cred, PublishKey: pk, WHIPURL: s.HTTP.URL + "/whip/" + pk,
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

// RefuseBind makes the next POST /api/v1/devices/bind answer with this problem (queued: one refusal per call), as
// Bot refuses a node token (401 bot.node_token_required) or a principal it will not bind (403 bot.node_not_bound).
func (s *Server) RefuseBind(status int, code, detail string) {
	s.mu.Lock()
	s.bindRefusals = append(s.bindRefusals, refusal{status, code, detail})
	s.mu.Unlock()
}

// Binds counts POST /api/v1/devices/bind requests.
func (s *Server) Binds() int { return int(s.binds.Load()) }

// bind is POST /api/v1/devices/bind: the bearer must be a registered credential (a node token the fake Network
// registered); the answer is POST /pair's without the credential, with a new publish key every call.
func (s *Server) bind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		problem(w, http.StatusMethodNotAllowed, "bot.method_not_allowed", "POST only")
		return
	}
	s.binds.Add(1)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	if len(s.bindRefusals) > 0 {
		rf := s.bindRefusals[0]
		s.bindRefusals = s.bindRefusals[1:]
		s.mu.Unlock()
		problem(w, rf.status, rf.code, rf.detail)
		return
	}
	dev, ok := s.creds[tok]
	robot := s.robots[dev]
	s.mu.Unlock()
	if !ok {
		problem(w, http.StatusUnauthorized, "bot.node_token_required", "a Network node token (audience openvibe.bot) is required")
		return
	}
	pk := "pk_" + token()[:16]
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(protocol.PairResponse{DeviceID: dev, PublishKey: pk, WHIPURL: s.HTTP.URL + "/whip/" + pk, RobotID: robot,
		Profile: json.RawMessage(`{"id":"sim.rover","limits":{"max_command_ms":300,"heartbeat_ms":1000}}`)})
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	s.dials.Add(1)
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
	if cfg.AllowedCommands == nil {
		cfg.AllowedCommands = append([]string(nil), protocol.Kinds...) // Bot always sends the list, halt included
	}
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

// Command sends a command the way Bot does: deadline_ms is the absolute instant ts + deadlineMS (none when 0), ref is
// the operator's own id, the operator is usr_test unless cmd names one, and robot_id is the device's robot.
func (c *Conn) Command(cmd protocol.Command, deadlineMS int) error {
	return c.CommandLate(cmd, deadlineMS, 0)
}

// CommandLate sends a command stamped late ago, as if it had sat in a queue on the way for that long: ts and the
// deadline are both in the past by late.
func (c *Conn) CommandLate(cmd protocol.Command, deadlineMS int, late time.Duration) error {
	ts := time.Now().Add(-late).UnixMilli()
	if deadlineMS > 0 {
		cmd.DeadlineMS = ts + int64(deadlineMS)
	}
	if cmd.Ref == "" {
		cmd.Ref = "op_" + cmd.ID
	}
	if cmd.Operator == nil {
		cmd.Operator = &protocol.Operator{Subject: "usr_test", Role: "operator"}
	}
	if cmd.RobotID == "" {
		cmd.RobotID = c.RobotID
	}
	return c.sendAt(ts, cmd)
}

// SendLate writes one frame stamped late ago, as if it had sat in a queue on the way for that long.
func (c *Conn) SendLate(m protocol.Message, late time.Duration) error {
	return c.sendAt(time.Now().Add(-late).UnixMilli(), m)
}

// SendError sends Bot's error frame.
func (c *Conn) SendError(code, detail string) error {
	return c.Send(protocol.Error{Code: code, Detail: detail})
}

// Job sends a job frame; the device answers ack or nack keyed by j.ID (see Reply).
func (c *Conn) Job(j protocol.Job) error { return c.Send(protocol.JobRequest{Job: j}) }

// JobCancel asks the device to stop job id.
func (c *Conn) JobCancel(id string) error { return c.Send(protocol.JobCancel{ID: id}) }

// JobExitAck tells the device job id's job_exit is recorded.
func (c *Conn) JobExitAck(id string) error { return c.Send(protocol.JobExitAck{ID: id}) }

// setEstop records the robot's latch: Bot keeps it from the estop frames it sends and the latched:true estop_state
// frames it receives.
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
		if es, ok := f.Msg.(protocol.EstopState); ok && es.Latched {
			c.setEstop(true) // a report of latched:false never clears Bot's latch
		}
		if hb, ok := f.Msg.(protocol.Heartbeat); ok && c.autoAck.Load() && !c.muted.Load() {
			// Bot answers with its own envelope seq (sendAt sets it): echo and t are the heartbeat's send time.
			ack := protocol.HeartbeatAck{ServerTime: time.Now().UTC().Format(time.RFC3339Nano)}
			if hb.T != 0 {
				t := hb.T
				ack.T, ack.Echo = t, &t
			}
			_ = c.Send(ack)
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
