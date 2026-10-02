// Package protocol holds every wire detail of the link between the Node and OpenVibe.Bot: the pairing request, the
// device WebSocket's frame envelope, each message type, fault codes, close codes and the endpoint paths. Nothing
// outside this package knows a JSON field name, so aligning with the service's docs/protocol.md is a change to this
// package only.
//
// Source of truth: OpenVibe.Bot docs/protocol.md and server/realtime.js (ADR-043 decisions 1, 2, 4, 5 and 6).
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	TypeEstop        = "estop"
	TypeHeartbeatAck = "heartbeat_ack"
	TypeError        = "error"
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

// WebSocket close codes the server uses on /device.
const (
	CloseReplaced      = 4000 // a newer connection of this device replaced this one
	CloseBadCredential = 4002 // the credential is unknown (or the rotated one's grace period is over)
	CloseRevoked       = 4003 // the owner revoked the device
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

// GuardedKinds move something: they carry a deadline and are refused while an e-stop or the local kill switch is
// latched.
var GuardedKinds = map[string]bool{KindDrive: true, KindActuator: true, KindPTZ: true}

// Fault codes carried in nack.fault_code and status.faults[].code.
const (
	FaultBadFrame      = "bad_frame"      // the command could not be parsed
	FaultBadValue      = "bad_value"      // a value is missing, not a number, or out of shape
	FaultUnsupported   = "unsupported"    // no driver on this device handles the kind or the actuator
	FaultNotAllowed    = "not_allowed"    // the kind is not in config.allowed_commands
	FaultExpired       = "expired"        // the command's deadline had passed when it arrived; it never ran
	FaultEstopped      = "estopped"       // the remote e-stop is latched
	FaultLocalStop     = "local_stop"     // the local kill switch (`openvibe-node stop`) is latched
	FaultNoHeartbeat   = "no_heartbeat"   // a plugin lost the core's heartbeat
	FaultNotReady      = "not_ready"      // the driver is starting or restarting, or the server's config has not arrived
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

// Hello opens every authenticated connection.
type Hello struct {
	SessionID  string   `json:"session_id,omitempty"`
	DeviceID   string   `json:"device_id,omitempty"`
	RobotIDs   []string `json:"robot_ids,omitempty"`
	ServerTime string   `json:"server_time,omitempty"` // ISO-8601, e.g. 2026-09-29T19:20:00.000Z
}

// Time parses ServerTime; ok is false when it is absent or malformed.
func (h Hello) Time() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, h.ServerTime)
	return t, err == nil && h.ServerTime != ""
}

type Limits struct {
	// MaxSpeed and MaxTurn are fractions of full scale (0..1). Nil means no owner limit (1.0).
	MaxSpeed     *float64 `json:"max_speed,omitempty"`
	MaxTurn      *float64 `json:"max_turn,omitempty"`
	MaxCommandMS int      `json:"max_command_ms,omitempty"`
}

// Config follows hello.
type Config struct {
	HeartbeatMS int    `json:"heartbeat_ms,omitempty"`
	Limits      Limits `json:"limits"`
	// AllowedCommands lists the kinds the robot accepts. Absent (nil) = no restriction; present = only these (halt is
	// always accepted: stopping is never refused).
	AllowedCommands []string `json:"allowed_commands"`
	// EstopLatched is the robot's e-stop on the server. Absent = unchanged.
	EstopLatched *bool `json:"estop_latched,omitempty"`
}

// Operator is who sent a command.
type Operator struct {
	Subject string `json:"subject,omitempty"`
	Role    string `json:"role,omitempty"`
}

type Command struct {
	ID         string          `json:"id"` // server-minted; the idempotency key on the device
	Ref        string          `json:"ref,omitempty"`
	Kind       string          `json:"kind"`
	Value      json.RawMessage `json:"value,omitempty"`
	DeadlineMS int64           `json:"deadline_ms,omitempty"` // absolute epoch ms; motion kinds only
	Operator   *Operator       `json:"operator,omitempty"`
	RobotID    string          `json:"robot_id,omitempty"`
	Target     string          `json:"target,omitempty"` // optional driver name; default: the first driver with the capability
}

// Estop is the server's e-stop: latched:true latches; latched:false is the owner's clear.
type Estop struct {
	Latched bool   `json:"latched"`
	By      string `json:"by,omitempty"`
	At      string `json:"at,omitempty"`
}

// HeartbeatAck answers a heartbeat. Bot echoes the heartbeat's seq in the envelope seq field (overwriting its own);
// Echo carries it instead once the server sends a separate correlation field.
type HeartbeatAck struct {
	Echo       *uint64 `json:"echo,omitempty"`
	ServerTime string  `json:"server_time,omitempty"`
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

// Status is sent once hello and config have arrived, and on every change. Bot reads firmware, capabilities, faults
// and estop_latched and shows the frame as it is; the other fields are for people reading it.
type Status struct {
	Firmware     string         `json:"firmware"`
	Capabilities map[string]any `json:"capabilities"`
	Faults       []Fault        `json:"faults"`
	EstopLatched bool           `json:"estop_latched"` // anything latched on the device (remote or local)
	LocalStop    bool           `json:"local_stop"`
	AgentVersion string         `json:"agent_version"`
	DeviceKind   string         `json:"device_kind,omitempty"`
	OS           string         `json:"os,omitempty"`
	Arch         string         `json:"arch,omitempty"`
	Drivers      []DriverStatus `json:"drivers"`
	Video        string         `json:"video,omitempty"`
}

type Event struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver,omitempty"`
	TS     int64          `json:"ts"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Telemetry is at most 2 Hz (the server drops faster frames). Battery is the charge as a fraction 0..1 and Voltage
// the pack voltage, as Bot's robot_state shows them.
type Telemetry struct {
	Battery *float64       `json:"battery,omitempty"`
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

// Heartbeat is sent every heartbeat_ms; RTTMS is the round trip measured on the previous one.
type Heartbeat struct {
	RTTMS *int64 `json:"rtt_ms,omitempty"`
}

// EstopState reports a latch held on the device (the local kill switch). Bot sets the robot's e-stop to Latched, so
// latched:false is only sent to lift a latch this device reported; see node.reportLatch. RobotID is the robot from
// hello's robot_ids when the device serves exactly one, else empty.
type EstopState struct {
	Latched bool   `json:"latched"`
	By      string `json:"by,omitempty"`
	At      string `json:"at,omitempty"`
	RobotID string `json:"robot_id,omitempty"`
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
	case *HeartbeatAck:
		return *v
	case *Error:
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

// DeviceKind values for PairRequest.DeviceKind.
const (
	DeviceOnboard = "onboard"
	DeviceBridge  = "bridge"
)

type PairRequest struct {
	// Robot (rob_…, from the installer command or QR) charges a wrong code to that robot's live code (5 tries end it).
	Robot        string         `json:"robot,omitempty"`
	Code         string         `json:"code"`
	AgentVersion string         `json:"agent_version"`
	DeviceKind   string         `json:"device_kind"`
	Drivers      []string       `json:"drivers"`
	Capabilities map[string]any `json:"capabilities"`
	Name         string         `json:"name,omitempty"` // shown to the owner; default: the hostname
	OS           string         `json:"os,omitempty"`
	Arch         string         `json:"arch,omitempty"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type PairResponse struct {
	DeviceID   string          `json:"device_id"`
	Credential string          `json:"credential"`
	PublishKey string          `json:"publish_key"`
	RobotID    string          `json:"robot_id,omitempty"`
	Profile    json.RawMessage `json:"profile,omitempty"`
	WHIPURL    string          `json:"whip_url,omitempty"`   // not sent by Bot yet; video starts only when present
	DeviceURL  string          `json:"device_url,omitempty"` // optional override of wss://<origin>/device
	ICEServers []ICEServer     `json:"ice_servers,omitempty"`
}

// Problem is an RFC 9457 problem+json body, Bot's shape for every HTTP error.
type Problem struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status int    `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"` // Bot repeats detail here for older clients
}

// Pairing problem codes (Bot domain/index.js redeem and the pair rate limit).
const (
	ProblemCodeShape   = "bot.invalid_pairing_code" // 422: not 8 characters
	ProblemNoCode      = "bot.no_pairing_code"      // 404: the robot has no live code
	ProblemCodeInvalid = "bot.pairing_code_invalid" // 403: wrong code (a try is charged when robot is sent)
	ProblemCodeLocked  = "bot.pairing_code_locked"  // 403: five wrong tries
	ProblemCodeUsed    = "bot.pairing_code_used"    // 403
	ProblemCodeExpired = "bot.pairing_code_expired" // 403: older than 10 minutes
	ProblemRateLimited = "rate_limited"             // 429
)
