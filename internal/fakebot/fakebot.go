// Package fakebot is a stand-in for OpenVibe.Bot used by tests. It speaks Bot's device protocol as Bot's own code does
// (server/realtime.js and server/api/v1.js at d398445), not through the Node's types: every server frame is built
// field by field the way realtime.js builds it, pairing refusals are RFC 9457 problems with Bot's codes, a bad
// credential is closed with 4002 after the upgrade, a replaced connection with 4000 and a revoked device with 4003.
// So a Node that decodes Bot's frames wrongly fails against it.
//
// Tests drive it to send commands, e-stops, errors and silence, and read back what the device sent.
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

// RobotID is the robot every fake device serves.
const RobotID = "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X"

// Bot's default owner command list (domain.control.allowedFor(robot, 'owner')).
var defaultAllowed = []string{"drive", "actuator", "ptz", "say", "display", "halt"}

// Server is the fake service.
type Server struct {
	HTTP *httptest.Server

	mu          sync.Mutex
	heartbeatMS int
	limits      map[string]any // the profile's limits, sent as config.limits
	allowed     []string
	estop       bool // the robot's e-stop on the server
	estopBy     string
	helloDelay  time.Duration
	whip        http.Handler
	codes       map[string]*code
	creds       map[string]string // credential → device id
	live        map[string]*Conn  // device id → its current connection
	pairReqs    []protocol.PairRequest
	credInURL   bool

	conns chan *Conn
	count atomic.Int64
}

type code struct {
	robot   string
	tries   int
	used    bool
	expired bool
}

// New starts the fake server.
func New() *Server {
	s := &Server{
		heartbeatMS: 1000, limits: map[string]any{"max_command_ms": 1000}, allowed: defaultAllowed,
		codes: map[string]*code{}, creds: map[string]string{}, live: map[string]*Conn{}, conns: make(chan *Conn, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PairPath, s.pair)
	mux.HandleFunc(protocol.DevicePath, s.device)
	mux.HandleFunc("/whip/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h := s.whip
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

// SetWHIP mounts a WHIP handler. Bot does not send whip_url yet; with a handler mounted the pairing answer carries one
// so video tests can run.
func (s *Server) SetWHIP(h http.Handler) {
	s.mu.Lock()
	s.whip = h
	s.mu.Unlock()
}

// SetConfig changes what later connections get in config: heartbeat_ms, the profile limits and allowed_commands
// (nil = Bot's default list).
func (s *Server) SetConfig(heartbeatMS int, limits map[string]any, allowed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if heartbeatMS > 0 {
		s.heartbeatMS = heartbeatMS
	}
	if limits != nil {
		s.limits = limits
	}
	if allowed == nil {
		allowed = defaultAllowed
	}
	s.allowed = allowed
}

// SetHelloDelay delays hello and config after the upgrade, as Bot's credential lookup does. A frame the device sends
// meanwhile is answered with error bot.not_paired and counted by NotPaired.
func (s *Server) SetHelloDelay(d time.Duration) {
	s.mu.Lock()
	s.helloDelay = d
	s.mu.Unlock()
}

// SetRobotEstop sets the robot's e-stop on the server without telling the device (an owner acting while it is
// offline); the next config carries it.
func (s *Server) SetRobotEstop(latched bool, by string) {
	s.mu.Lock()
	s.estop, s.estopBy = latched, by
	s.mu.Unlock()
}

// RobotEstop is the robot's e-stop on the server and who set it ("device" when the device's estop_state did).
func (s *Server) RobotEstop() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estop, s.estopBy
}

// AddCode makes a one-time pairing code valid (any robot).
func (s *Server) AddCode(c string) { s.AddRobotCode("", c) }

// AddRobotCode makes c the live pairing code of robot.
func (s *Server) AddRobotCode(robot, c string) {
	s.mu.Lock()
	s.codes[normalize(c)] = &code{robot: robot}
	s.mu.Unlock()
}

// ExpireCode makes a code older than its 10 minutes.
func (s *Server) ExpireCode(c string) {
	s.mu.Lock()
	if k := s.codes[normalize(c)]; k != nil {
		k.expired = true
	}
	s.mu.Unlock()
}

// AddCredential registers a credential directly (skipping pairing).
func (s *Server) AddCredential(cred, deviceID string) {
	s.mu.Lock()
	s.creds[cred] = deviceID
	s.mu.Unlock()
}

// Revoke invalidates a credential and closes its device's connection with 4003, as Bot's closeDevice does.
func (s *Server) Revoke(cred string) {
	s.mu.Lock()
	dev := s.creds[cred]
	delete(s.creds, cred)
	c := s.live[dev]
	s.mu.Unlock()
	if c != nil {
		c.CloseWith(protocol.CloseRevoked, "revoked")
	}
}

// Rotate issues a new credential and publish key for old's device and returns the answer of
// POST /api/v1/devices/<id>/rotate. The old credential keeps working for grace.
func (s *Server) Rotate(old string, grace time.Duration) []byte {
	s.mu.Lock()
	dev := s.creds[old]
	cred := token()
	s.creds[cred] = dev
	s.mu.Unlock()
	time.AfterFunc(grace, func() {
		s.mu.Lock()
		delete(s.creds, old)
		s.mu.Unlock()
	})
	b, _ := json.Marshal(map[string]any{"device": map[string]any{"id": dev}, "credential": cred, "publish_key": token()})
	return b
}

// PairRequests returns what devices sent to /api/v1/pair.
func (s *Server) PairRequests() []protocol.PairRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.PairRequest(nil), s.pairReqs...)
}

// CredentialSeenInURL reports whether any request carried the credential in its URL.
func (s *Server) CredentialSeenInURL() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credInURL
}

// Connections counts device WebSocket upgrades (refused credentials included).
func (s *Server) Connections() int { return int(s.count.Load()) }

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func normalize(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(c)))
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// problem answers like Bot's http.sendProblem: RFC 9457 with Bot's code and detail.
func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code, "detail": detail,
	})
}

// pair is POST /api/v1/pair with domain.pairing.redeem's refusals.
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
	normal := normalize(req.Code)
	if len(normal) != 8 {
		problem(w, http.StatusUnprocessableEntity, "bot.invalid_pairing_code", "the pairing code must be 8 characters (XXXX-XXXX)")
		return
	}
	s.mu.Lock()
	s.pairReqs = append(s.pairReqs, req)
	status, pcode, detail := s.redeemLocked(req.Robot, normal)
	var cred, dev string
	whip := s.whip != nil
	if status == 0 {
		cred, dev = token(), "dev_"+token()[:12]
		s.creds[cred] = dev
	}
	s.mu.Unlock()
	if status != 0 {
		problem(w, status, pcode, detail)
		return
	}
	body := map[string]any{"device_id": dev, "credential": cred, "publish_key": token(), "robot_id": RobotID,
		"profile": map[string]any{"id": "sim.rover", "capabilities": []string{"drive"}}}
	if whip {
		body["whip_url"] = s.HTTP.URL + "/whip/" + dev
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) redeemLocked(robot, normal string) (int, string, string) {
	var row *code
	if robot != "" {
		for c, k := range s.codes {
			if k.robot == robot {
				row = k
				if c != normal {
					k.tries++
					if k.tries >= 5 {
						k.used = true
						return 403, "bot.pairing_code_locked", "too many wrong tries; the code is dead"
					}
					return 403, "bot.pairing_code_invalid", "that is not the pairing code"
				}
			}
		}
		if row == nil {
			return 404, "bot.no_pairing_code", "this robot has no pairing code; ask the owner for a new one"
		}
	} else if row = s.codes[normal]; row == nil || row.used {
		return 403, "bot.pairing_code_invalid", "that is not a live pairing code"
	}
	switch {
	case row.used:
		return 403, "bot.pairing_code_used", "that pairing code has already been used"
	case row.expired:
		return 403, "bot.pairing_code_expired", "that pairing code has expired"
	case row.tries >= 5:
		return 403, "bot.pairing_code_locked", "too many wrong tries; the code is dead"
	}
	row.used = true
	return 0, "", ""
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// device is /device: upgrade first, then authenticate (Bot closes a bad credential with 4002 after the upgrade).
func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	for cred := range s.creds {
		if strings.Contains(r.URL.RawQuery, cred) || strings.Contains(r.URL.Path, cred) {
			s.credInURL = true
		}
	}
	auth := r.Header.Get("Authorization")
	dev, ok := s.creds[strings.TrimPrefix(auth, "Bearer ")]
	ok = ok && strings.HasPrefix(auth, "Bearer ")
	delay := s.helloDelay
	s.mu.Unlock()
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.count.Add(1)
	c := &Conn{srv: s, ws: ws, DeviceID: dev, recv: make(chan protocol.Frame, 1024), done: make(chan struct{})}
	c.pending.Store(true)
	c.autoAck.Store(true)
	c.rttMS.Store(-1)
	go c.read()
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		c.CloseWith(protocol.CloseBadCredential, "invalid credential")
		return
	}
	s.mu.Lock()
	old := s.live[dev]
	s.live[dev] = c
	heartbeatMS, limits, allowed, estop := s.heartbeatMS, s.limits, s.allowed, s.estop
	s.mu.Unlock()
	if old != nil {
		old.CloseWith(protocol.CloseReplaced, "replaced by a newer connection")
	}
	c.pending.Store(false)
	// sendHello and sendConfig, field for field.
	_ = c.SendFrame("hello", map[string]any{"session_id": "sess_" + token()[:12], "device_id": dev,
		"robot_ids": []string{RobotID}, "server_time": iso(time.Now())})
	_ = c.SendFrame("config", map[string]any{"heartbeat_ms": heartbeatMS, "limits": limits, "allowed_commands": allowed,
		"estop_latched": estop})
	select {
	case s.conns <- c:
	default:
	}
}

// NextConn waits for the next authenticated device connection.
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
	DeviceID  string
	srv       *Server
	ws        *websocket.Conn
	wmu       sync.Mutex
	seq       uint64
	recv      chan protocol.Frame
	done      chan struct{}
	pending   atomic.Bool
	autoAck   atomic.Bool
	muted     atomic.Bool
	notPaired atomic.Int64
	rttMS     atomic.Int64 // the device's last rtt_ms, -1 = none yet
	frames    []protocol.Frame
	fmu       sync.Mutex
}

// SendFrame writes {v, seq, ts, type, ...fields} as Bot's sendFrame does (a field named seq overwrites the envelope's).
func (c *Conn) SendFrame(typ string, fields map[string]any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	f := map[string]any{"v": 1, "seq": c.seq, "ts": time.Now().UnixMilli(), "type": typ}
	for k, v := range fields {
		f[k] = v
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// Cmd is an operator command as Bot forwards it.
type Cmd struct {
	ID       string // default: a fresh cmd_… id, as Bot mints one per command
	Kind     string
	Value    any       // marshalled as is; a json.RawMessage is sent verbatim
	Deadline time.Time // absolute, sent as epoch ms; zero = no deadline (non-motion kinds)
	Subject  string    // default usr_operator
	Role     string    // default operator
}

// Command sends a command frame shaped like sendOperatorCommand's and returns its id.
func (c *Conn) Command(cmd Cmd) (string, error) {
	if cmd.ID == "" {
		cmd.ID = "cmd_" + token()[:20]
	}
	if cmd.Subject == "" {
		cmd.Subject = "usr_operator"
	}
	if cmd.Role == "" {
		cmd.Role = "operator"
	}
	var deadline any // null when absent, as decision.deadlineMs is for say/display
	if !cmd.Deadline.IsZero() {
		deadline = cmd.Deadline.UnixMilli()
	}
	return cmd.ID, c.SendFrame("command", map[string]any{"id": cmd.ID, "ref": "op_" + token()[:8], "kind": cmd.Kind,
		"value": cmd.Value, "deadline_ms": deadline, "operator": map[string]any{"subject": cmd.Subject, "role": cmd.Role},
		"robot_id": RobotID})
}

// Estop sets or clears the robot's e-stop as onOperatorEstop does and tells the device.
func (c *Conn) Estop(latched bool, by string) error {
	c.srv.SetRobotEstop(latched, by)
	return c.SendFrame("estop", map[string]any{"latched": latched, "by": by, "at": iso(time.Now())})
}

// SendError sends Bot's error frame.
func (c *Conn) SendError(code, detail string) error {
	return c.SendFrame("error", map[string]any{"code": code, "detail": detail})
}

// SendRaw writes arbitrary bytes (for malformed-frame tests).
func (c *Conn) SendRaw(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// CloseWith closes the connection with a close code, as Bot's ws.close(code, reason) does.
func (c *Conn) CloseWith(code int, reason string) {
	c.wmu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	c.wmu.Unlock()
	_ = c.ws.Close()
}

// Mute stops answering heartbeats and sending anything, as a hung server would.
func (c *Conn) Mute() { c.muted.Store(true); c.autoAck.Store(false) }

// Close drops the connection without a close frame.
func (c *Conn) Close() { _ = c.ws.Close() }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// NotPaired counts frames the device sent before hello (answered with error bot.not_paired).
func (c *Conn) NotPaired() int { return int(c.notPaired.Load()) }

// RTT is the device's last reported rtt_ms; ok is false until one arrived.
func (c *Conn) RTT() (int64, bool) {
	v := c.rttMS.Load()
	return v, v >= 0
}

// read handles device frames as handleDeviceMessage does, and records them decoded.
func (c *Conn) read() {
	defer close(c.done)
	defer func() {
		c.srv.mu.Lock()
		if c.srv.live[c.DeviceID] == c {
			delete(c.srv.live, c.DeviceID)
		}
		c.srv.mu.Unlock()
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var raw map[string]any
		if json.Unmarshal(data, &raw) != nil {
			_ = c.SendError("bot.bad_json", "frames must be JSON")
			continue
		}
		if c.pending.Load() {
			c.notPaired.Add(1)
			_ = c.SendError("bot.not_paired", "send pair first")
			continue
		}
		switch raw["type"] {
		case "heartbeat":
			if rtt, ok := raw["rtt_ms"].(float64); ok {
				c.rttMS.Store(int64(rtt + 0.5))
			}
			if c.autoAck.Load() && !c.muted.Load() {
				_ = c.SendFrame("heartbeat_ack", map[string]any{"seq": raw["seq"], "server_time": iso(time.Now())})
			}
		case "estop_state":
			// onEstopState: the robot's e-stop follows the device's report.
			latched, _ := raw["latched"].(bool)
			if on, _ := c.srv.RobotEstop(); on != latched {
				c.srv.SetRobotEstop(latched, "device")
			}
		case "telemetry", "status", "ack", "nack":
		default:
			_ = c.SendError("bot.unknown_message", fmt.Sprintf("unknown type %v", raw["type"]))
		}
		f, err := protocol.Decode(data)
		if err != nil {
			continue
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
