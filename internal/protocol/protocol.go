// Package protocol holds every wire detail of the link between the Node and OpenVibe.Bot: the pairing request, the
// device WebSocket's frame envelope, each message type, fault codes and the endpoint paths. Nothing outside this
// package knows a JSON field name, so aligning with the service's docs/protocol.md is a change to this package only.
//
// Source of truth: ADR-043 decisions 1, 2, 4, 5 and 6 (bot.device-message@1).
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
	TypeEstop        = "estop"
	TypeEstopClear   = "estop_clear"
	TypeHeartbeatAck = "heartbeat_ack"
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
	FaultExpired       = "expired"        // the deadline had already passed on arrival
	FaultNotReady      = "not_ready"      // the driver is starting or restarting
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
	DeviceID   string `json:"device_id,omitempty"`
	RobotID    string `json:"robot_id,omitempty"`
	Session    string `json:"session,omitempty"`
	ServerTime int64  `json:"server_time,omitempty"`
}

type Limits struct {
	// MaxSpeed and MaxTurn are fractions of full scale (0..1). Nil means no owner limit (1.0).
	MaxSpeed     *float64 `json:"max_speed,omitempty"`
	MaxTurn      *float64 `json:"max_turn,omitempty"`
	MaxCommandMS int      `json:"max_command_ms,omitempty"`
}

type Config struct {
	Limits      Limits   `json:"limits"`
	HeartbeatMS int      `json:"heartbeat_ms,omitempty"`
	DeadmanMS   int      `json:"deadman_ms,omitempty"`
	TelemetryMS int      `json:"telemetry_ms,omitempty"`
	Allowed     []string `json:"allowed,omitempty"` // command kinds the current operator may send; empty = all
}

type Command struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Value      json.RawMessage `json:"value,omitempty"`
	DeadlineMS int             `json:"deadline_ms,omitempty"` // milliseconds from receipt; see docs/protocol.md
	Operator   string          `json:"operator,omitempty"`
	Role       string          `json:"role,omitempty"`
	Target     string          `json:"target,omitempty"` // optional driver name; default: the first driver with the capability
}

type Estop struct {
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
}

type EstopClear struct {
	By string `json:"by,omitempty"`
}

type HeartbeatAck struct {
	T int64 `json:"t"`
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

type Battery struct {
	Volts   *float64 `json:"volts,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
}

type Event struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver,omitempty"`
	TS     int64          `json:"ts"`
	Fields map[string]any `json:"fields,omitempty"`
}

type Telemetry struct {
	Battery *Battery       `json:"battery,omitempty"`
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

type Heartbeat struct {
	T int64 `json:"t"`
}

type EstopState struct {
	Latched   bool   `json:"latched"`
	LocalStop bool   `json:"local_stop"`
	Reason    string `json:"reason,omitempty"`
}

func (Hello) MessageType() string        { return TypeHello }
func (Config) MessageType() string       { return TypeConfig }
func (Command) MessageType() string      { return TypeCommand }
func (Estop) MessageType() string        { return TypeEstop }
func (EstopClear) MessageType() string   { return TypeEstopClear }
func (HeartbeatAck) MessageType() string { return TypeHeartbeatAck }
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
	TypeEstopClear:   func() Message { return &EstopClear{} },
	TypeHeartbeatAck: func() Message { return &HeartbeatAck{} },
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
	case *EstopClear:
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

// DeviceKind values for PairRequest.DeviceKind.
const (
	DeviceOnboard = "onboard"
	DeviceBridge  = "bridge"
)

type PairRequest struct {
	Code         string         `json:"code"`
	AgentVersion string         `json:"agent_version"`
	DeviceKind   string         `json:"device_kind"`
	Drivers      []string       `json:"drivers"`
	Capabilities map[string]any `json:"capabilities"`
	OS           string         `json:"os,omitempty"`
	Arch         string         `json:"arch,omitempty"`
	Hostname     string         `json:"hostname,omitempty"`
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
	Profile    json.RawMessage `json:"profile,omitempty"`
	WHIPURL    string          `json:"whip_url,omitempty"`
	DeviceURL  string          `json:"device_url,omitempty"` // optional override of wss://<origin>/device
	ICEServers []ICEServer     `json:"ice_servers,omitempty"`
}

// PairError is the body of a non-2xx pairing answer.
type PairError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}
