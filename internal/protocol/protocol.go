// Package protocol holds every wire detail of the link between the Node and OpenVibe.Bot: the pairing request, the
// device WebSocket's frame envelope, each message type, fault codes and the endpoint paths. Nothing outside this
// package knows a JSON field name, so aligning with the service's docs/protocol.md is a change to this package only.
//
// Source of truth: OpenVibe.Bot docs/protocol.md section 1 (the /device socket) and its POST /pair; the exact frames
// are pinned in testdata/bot (see its README for the Bot commit).
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the envelope version, the `v` field of every frame.
const Version = 1

// Endpoint paths, relative to the server origin (default https://openvibe.bot).
const (
	PairPath   = "/api/v1/pair"
	DevicePath = "/device"
)

// Server → device message types.
const (
	TypeHello        = "hello"
	TypeConfig       = "config"
	TypeCommand      = "command"
	TypeEstop        = "estop" // latched true sets the e-stop, false is the owner clearing it
	TypeHeartbeatAck = "heartbeat_ack"
	TypeError        = "error"
)

// Close codes the server ends the device socket with.
const (
	CloseReplaced = 4000 // a second connection with the same credential replaced this one: reconnect normally
	CloseInvalid  = 4002 // the credential is wrong, rotated or revoked (sent right after the upgrade)
	CloseRevoked  = 4003 // the owner revoked the device (or rotated its credential) while it was connected
)

// Error codes in error.code that the Node tells apart.
const (
	ErrNotPaired = "bot.not_paired" // a frame arrived on a socket that has no credential
	ErrNotReady  = "bot.not_ready"  // more than 64 frames arrived while the server was still authenticating
)

// Device → server message types.
const (
	TypeStatus     = "status"
	TypeTelemetry  = "telemetry"
	TypeAck        = "ack"
	TypeNack       = "nack"
	TypeHeartbeat  = "heartbeat"
	TypeEstopState = "estop_state"
)

// Command kinds.
const (
	KindDrive    = "drive"
	KindActuator = "actuator"
	KindPTZ      = "ptz"
	KindSay      = "say"
	KindDisplay  = "display"
	KindHalt     = "halt"
)

// Kinds lists every command kind the Node understands.
var Kinds = []string{KindDrive, KindActuator, KindPTZ, KindSay, KindDisplay, KindHalt}

// GuardedKinds move something and are refused while an e-stop or the local kill switch is latched.
var GuardedKinds = map[string]bool{KindDrive: true, KindActuator: true, KindPTZ: true}

// Fault codes carried in nack.fault_code and status.faults[].code.
const (
	FaultBadFrame      = "bad_frame"      // the command could not be parsed
	FaultBadValue      = "bad_value"      // a value is missing, not a number, or out of shape
	FaultUnsupported   = "unsupported"    // no driver on this device handles the kind or the actuator
	FaultNotAllowed    = "not_allowed"    // the kind is not in config.allowed
	FaultEstopped      = "estopped"       // the remote e-stop is latched
	FaultLocalStop     = "local_stop"     // the local kill switch (`openvibe-node stop`) is latched
	FaultNoHeartbeat   = "no_heartbeat"   // a plugin lost the core's heartbeat
	FaultNotReady      = "not_ready"      // the driver is starting or restarting, or the server's config has not arrived
	FaultExpired       = "expired"        // the command's deadline_ms had passed when it arrived
	FaultPluginDown    = "plugin_down"    // the driver process is not running
	FaultPluginTimeout = "plugin_timeout" // the driver did not answer in time
	FaultHardware      = "hardware"       // the driver reported a hardware error
	FaultFirmware      = "firmware_unsupported"
	FaultNotConnected  = "not_connected" // a bridge driver cannot reach its robot
	FaultCliff         = "cliff"         // refused because the robot is at a cliff edge
	FaultShuttingDown  = "shutting_down"
)

// DefaultDeadlineMS is used when a motion command carries no deadline.
const DefaultDeadlineMS = 300

// DefaultMaxCommandMS caps deadlines when the profile sets no max_command_ms.
const DefaultMaxCommandMS = 1000

// DefaultHeartbeatMS is the device heartbeat interval; the link is declared lost after two intervals without a frame.
const DefaultHeartbeatMS = 1000

// Message is any typed payload.
type Message interface{ MessageType() string }

// Envelope is the part common to every frame.
type Envelope struct {
	V    int    `json:"v"`
	Seq  uint64 `json:"seq"`
	TS   int64  `json:"ts"`
	Type string `json:"type"`
}

// Frame is a decoded frame: the envelope plus the typed message.
type Frame struct {
	Envelope
	Msg Message
}

// ---- server → device ----

type Hello struct {
	SessionID  string   `json:"session_id"`
	DeviceID   string   `json:"device_id"`
	RobotIDs   []string `json:"robot_ids"`
	ServerTime string   `json:"server_time"` // RFC 3339
}

type Limits struct {
	// MaxSpeed and MaxTurn are fractions of full scale (0..1). Nil means no owner limit (1.0).
	MaxSpeed     *float64 `json:"max_speed,omitempty"`
	MaxTurn      *float64 `json:"max_turn,omitempty"`
	MaxCommandMS int      `json:"max_command_ms,omitempty"`
	HeartbeatMS  int      `json:"heartbeat_ms,omitempty"`
}

type Config struct {
	HeartbeatMS     int      `json:"heartbeat_ms"`
	Limits          Limits   `json:"limits"`
	// AllowedCommands are the kinds the robot takes (Bot always includes halt). Absent (nil) allows every kind; an
	// empty list allows only halt.
	AllowedCommands []string `json:"allowed_commands"`
	EstopLatched    bool     `json:"estop_latched"`
}

// Operator is who sent a command, as the server's gate saw them.
type Operator struct {
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

type Command struct {
	// ID is minted by the server: the dedup and ack key. Ref is the operator's own id, for logs only.
	ID    string          `json:"id"`
	Ref   string          `json:"ref,omitempty"`
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value,omitempty"`
	// DeadlineMS is an absolute instant in epoch milliseconds on the server's clock, present on motion kinds only.
	DeadlineMS int64     `json:"deadline_ms,omitempty"`
	Operator   *Operator `json:"operator,omitempty"`
	RobotID    string    `json:"robot_id,omitempty"`
	Target     string    `json:"target,omitempty"` // Node only: a driver name; default: the first driver with the capability
}

// Estop is sent when the e-stop latches (Latched true) or the owner clears it (Latched false).
type Estop struct {
	Latched bool   `json:"latched"`
	By      string `json:"by,omitempty"`
	At      string `json:"at,omitempty"` // RFC 3339
}

// HeartbeatAck echoes the heartbeat's seq. The server writes it over the envelope's seq, so the decoded frame's
// Seq is the echoed value too.
type HeartbeatAck struct {
	Seq        uint64 `json:"seq"`
	ServerTime string `json:"server_time,omitempty"`
}

// Error is a frame the server refused.
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// ---- device → server ----

type DriverStatus struct {
	Name         string         `json:"name"`
	Driver       string         `json:"driver,omitempty"`
	Version      string         `json:"version,omitempty"`
	State        string         `json:"state"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

type Fault struct {
	Code    string `json:"code"`
	Driver  string `json:"driver,omitempty"`
	Message string `json:"message,omitempty"`
}

type Status struct {
	Firmware     string         `json:"firmware"`     // Bot: the agent's own version string
	Capabilities map[string]any `json:"capabilities"` // Bot: per driver, as in the pairing request
	AgentVersion string         `json:"agent_version"`
	DeviceKind   string         `json:"device_kind,omitempty"`
	OS           string         `json:"os,omitempty"`
	Arch         string         `json:"arch,omitempty"`
	Drivers      []DriverStatus `json:"drivers"`
	Faults       []Fault        `json:"faults"`
	EstopLatched bool           `json:"estop_latched"`
	LocalStop    bool           `json:"local_stop"`
	Video        string         `json:"video,omitempty"`
}

type Event struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver,omitempty"`
	TS     int64          `json:"ts"`
	Fields map[string]any `json:"fields,omitempty"`
}

type Telemetry struct {
	Battery *float64       `json:"battery,omitempty"` // charge, 0..1
	Voltage *float64       `json:"voltage,omitempty"`
	RSSI    *int           `json:"rssi,omitempty"`
	Sensors map[string]any `json:"sensors,omitempty"`
	Events  []Event        `json:"events,omitempty"`
}

type Ack struct {
	ID        string `json:"id"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

type Nack struct {
	ID        string `json:"id"`
	FaultCode string `json:"fault_code"`
	Message   string `json:"message,omitempty"`
}

// Heartbeat carries the same seq as its envelope (the link sets it), so the server's echo is unambiguous.
type Heartbeat struct {
	Seq   uint64 `json:"seq"`
	RTTMS *int64 `json:"rtt_ms,omitempty"`
}

type EstopState struct {
	Latched   bool   `json:"latched"`
	By        string `json:"by,omitempty"`
	At        string `json:"at,omitempty"` // RFC 3339
	RobotID   string `json:"robot_id,omitempty"`
	LocalStop bool   `json:"local_stop"`
	Reason    string `json:"reason,omitempty"`
}

func (Hello) MessageType() string        { return TypeHello }
func (Config) MessageType() string       { return TypeConfig }
func (Command) MessageType() string      { return TypeCommand }
func (Estop) MessageType() string        { return TypeEstop }
func (HeartbeatAck) MessageType() string { return TypeHeartbeatAck }
func (Error) MessageType() string        { return TypeError }
func (Status) MessageType() string       { return TypeStatus }
func (Telemetry) MessageType() string    { return TypeTelemetry }
func (Ack) MessageType() string          { return TypeAck }
func (Nack) MessageType() string         { return TypeNack }
func (Heartbeat) MessageType() string    { return TypeHeartbeat }
func (EstopState) MessageType() string   { return TypeEstopState }

var registry = map[string]func() Message{
	TypeHello:        func() Message { return &Hello{} },
	TypeConfig:       func() Message { return &Config{} },
	TypeCommand:      func() Message { return &Command{} },
	TypeEstop:        func() Message { return &Estop{} },
	TypeHeartbeatAck: func() Message { return &HeartbeatAck{} },
	TypeError:        func() Message { return &Error{} },
	TypeStatus:       func() Message { return &Status{} },
	TypeTelemetry:    func() Message { return &Telemetry{} },
	TypeAck:          func() Message { return &Ack{} },
	TypeNack:         func() Message { return &Nack{} },
	TypeHeartbeat:    func() Message { return &Heartbeat{} },
	TypeEstopState:   func() Message { return &EstopState{} },
}

// ErrUnknownType is returned (wrapped) by Decode for a frame whose type is not in this version's message set.
var ErrUnknownType = errors.New("unknown frame type")

// Encode builds one frame: the message's fields plus v, seq, ts and type at the top level.
func Encode(seq uint64, ts int64, m Message) ([]byte, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	env, err := json.Marshal(Envelope{V: Version, Seq: seq, TS: ts, Type: m.MessageType()})
	if err != nil {
		return nil, err
	}
	// Splice the two objects: {"v":..,"type":..} + {"id":..} → {"v":..,"type":..,"id":..}.
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' {
		return nil, fmt.Errorf("message %s did not encode as an object", m.MessageType())
	}
	if len(body) == 2 { // {}
		return env, nil
	}
	out := make([]byte, 0, len(env)+len(body))
	out = append(out, env[:len(env)-1]...)
	out = append(out, ',')
	out = append(out, body[1:]...)
	return out, nil
}

// Decode parses a frame. Unknown types return ErrUnknownType with the envelope filled, so the caller can ignore
// frames from a newer server.
func Decode(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f.Envelope); err != nil {
		return f, fmt.Errorf("%s: %w", FaultBadFrame, err)
	}
	if f.V != Version {
		return f, fmt.Errorf("%s: unsupported version %d", FaultBadFrame, f.V)
	}
	mk, ok := registry[f.Type]
	if !ok {
		return f, fmt.Errorf("%w %q", ErrUnknownType, f.Type)
	}
	m := mk()
	if err := json.Unmarshal(data, m); err != nil {
		return f, fmt.Errorf("%s: %s: %w", FaultBadFrame, f.Type, err)
	}
	f.Msg = derefMessage(m)
	return f, nil
}

func derefMessage(m Message) Message {
	switch v := m.(type) {
	case *Hello:
		return *v
	case *Config:
		return *v
	case *Command:
		return *v
	case *Estop:
		return *v
	case *Error:
		return *v
	case *HeartbeatAck:
		return *v
	case *Status:
		return *v
	case *Telemetry:
		return *v
	case *Ack:
		return *v
	case *Nack:
		return *v
	case *Heartbeat:
		return *v
	case *EstopState:
		return *v
	}
	return m
}

// ---- pairing (HTTPS) ----

// PairBodyFromPaired turns Bot's `paired` frame (pairing over the socket) into the body its POST /api/v1/pair answers
// with: the same device id, credential, publish key, WHIP URL and profile, and robot_ids[0] as robot_id
// (server/api/v1.js).
func PairBodyFromPaired(frame []byte) []byte {
	var p struct {
		DeviceID   string          `json:"device_id"`
		Credential string          `json:"credential"`
		PublishKey string          `json:"publish_key"`
		WHIPURL    string          `json:"whip_url"`
		RobotIDs   []string        `json:"robot_ids"`
		Profile    json.RawMessage `json:"profile"`
	}
	if json.Unmarshal(frame, &p) != nil {
		return nil
	}
	r := PairResponse{DeviceID: p.DeviceID, Credential: p.Credential, PublishKey: p.PublishKey, WHIPURL: p.WHIPURL, Profile: p.Profile}
	if len(p.RobotIDs) > 0 {
		r.RobotID = p.RobotIDs[0]
	}
	b, _ := json.Marshal(r)
	return b
}

// DeviceKind values for PairRequest.DeviceKind.
const (
	DeviceOnboard = "onboard"
	DeviceBridge  = "bridge"
)

// PairRequest is the body of POST /api/v1/pair (the same fields as Bot's `pair` frame, without the envelope).
type PairRequest struct {
	Robot        string         `json:"robot,omitempty"` // rob_… from the installer command; attributes a wrong try to that robot's code
	Code         string         `json:"code"`
	AgentVersion string         `json:"agent_version"`
	DeviceKind   string         `json:"device_kind"`
	Drivers      []string       `json:"drivers"`
	Capabilities map[string]any `json:"capabilities"`
	Name         string         `json:"name,omitempty"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// PairResponse is the 201 answer of POST /api/v1/pair. The credential, publish key and WHIP URL (which embeds the
// publish key; null when Bot has no ingest configured) appear only here.
type PairResponse struct {
	DeviceID   string          `json:"device_id"`
	Credential string          `json:"credential"`
	PublishKey string          `json:"publish_key"`
	WHIPURL    string          `json:"whip_url,omitempty"`
	RobotID    string          `json:"robot_id"`
	Profile    json.RawMessage `json:"profile,omitempty"`
}

// Problem is the RFC 9457 body of a non-2xx answer from Bot's REST API.
type Problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"` // e.g. bot.pairing_code_invalid
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
}
