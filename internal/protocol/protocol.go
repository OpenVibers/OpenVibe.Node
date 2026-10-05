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
	"regexp"
)

// Version is the envelope version, the `v` field of every frame.
const Version = 1

// Endpoint paths, relative to the server origin (default https://openvibe.bot).
const (
	PairPath   = "/api/v1/pair"
	DevicePath = "/device"
	// BindPath binds a Network-paired machine (its node token, audience openvibe.bot) to its device on Bot.
	BindPath = "/api/v1/devices/bind"
)

// OpenVibe.Network paths, relative to the Network origin (server/registry/node-principals.js).
const (
	NodePairingPath = "/api/v1/node-pairing" // redeems a Network pairing code for a node principal and its credential
	TokenPath       = "/oauth/token"         // client_credentials: the node credential buys a node token
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
	TypeReauth     = "reauth" // a Network-paired machine's fresh node token, before the last one expires
	TypeEstopState = "estop_state"
)

// Job frames (OpenVibe.Contracts platform.job-frame@1). Server → device: job, job_cancel, job_exit_ack. Device →
// server: job_started, job_stdout, job_usage, job_exit. A job is accepted or refused with ack/nack keyed by its id.
const (
	TypeJob        = "job"
	TypeJobCancel  = "job_cancel"
	TypeJobExitAck = "job_exit_ack"
	TypeJobStarted = "job_started"
	TypeJobStdout  = "job_stdout"
	TypeJobUsage   = "job_usage"
	TypeJobExit    = "job_exit"
)

// Runtime classes (platform.runtime-class@1).
const (
	ClassFunction = "function"
	ClassCode     = "code"
	ClassBrowser  = "browser"
	ClassLinux    = "linux"
	ClassDesktop  = "desktop"
	ClassGPU      = "gpu"
)

// RuntimeClasses lists every class the contract names; a worker runs only those it advertises.
var RuntimeClasses = []string{ClassFunction, ClassCode, ClassBrowser, ClassLinux, ClassDesktop, ClassGPU}

// Job network policies (platform.job@1 net). A job that names none runs under NetDeny: the contract's declared
// default, which is never widened by the Node's own worker.egress.
const (
	NetDeny         = "deny"          // no network at all; also what an absent net means
	NetNone         = "none"          // NetDeny under the name the Node's worker.egress uses
	NetPublic       = "public"        // public IPv4, filtered by the host
	NetOpenVibeOnly = "openvibe-only" // public, restricted to the host's egress_allow
)

// CapWorker is the status.capabilities key a worker advertises its runtime classes under (WorkerCapabilities). It is
// present only when the Node runs at least one class.
const CapWorker = "worker"

// WorkerCapPrefix is the namespace of the reserved Fabric capability names (platform.resource-offer@1.capabilities)
// a Node's runtime classes map to, one name per class: "worker:function", "worker:code", …. A name matches the
// offer's capabilities pattern ^[a-z][a-z0-9-]*:[a-z0-9.-]+$ with no schema change needed.
const WorkerCapPrefix = "worker:"

// WorkerCapabilityNames returns the reserved Fabric capability names for the given runtime classes, in order.
func WorkerCapabilityNames(classes []string) []string {
	names := make([]string, 0, len(classes))
	for _, c := range classes {
		names = append(names, WorkerCapPrefix+c)
	}
	return names
}

// Job refusal reasons, carried in nack.message (nack.fault_code is bad_value, bad_frame or unsupported).
const (
	JobBadID          = "missing or malformed job id"
	JobBadFrame       = "malformed job"
	JobUnknownClass   = "unknown class"
	JobNoArtifact     = "function, code or linux job needs an artifact with an exact version"
	JobNetUnsupported = "net policy not supported"
	JobBadInputs      = "invalid inputs"
	JobInputsRefused  = "job inputs not supported"
	JobBadLimits      = "missing or invalid args, ttl_ms or limits"
	JobNotAvailable   = "class not available"

	// Refused by a running worker (platform.job@1: a missing artifact, the local kill switch, limits it cannot honor).
	JobUnknownArtifact = "unknown artifact"                    // not a function declared in the Node's local config
	JobStopped         = "the e-stop or local stop is latched" // fault estopped or local_stop
	JobBusy            = "worker busy"                         // worker.caps.max_jobs are running; fault not_ready
)

// job_exit reasons.
const (
	ExitExited    = "exited"
	ExitCancelled = "cancelled"
	ExitTTL       = "ttl"
	ExitLimit     = "limit"
	ExitStopped   = "stopped"
	ExitFailed    = "failed"
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
	HeartbeatMS int    `json:"heartbeat_ms"`
	Limits      Limits `json:"limits"`
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

// HeartbeatAck answers a heartbeat. Bot's envelope seq is its own counter and never the device's, so the body
// carries the device's send time back as t (and, on newer Bot, echo: the same value, or null when the heartbeat
// had no t) and Bot's clock in server_time. Seq is present only from older servers that echoed the heartbeat's seq.
type HeartbeatAck struct {
	Seq        uint64 `json:"seq,omitempty"`
	T          int64  `json:"t,omitempty"`
	Echo       *int64 `json:"echo,omitempty"`
	ServerTime string `json:"server_time,omitempty"`
}

// Error is a frame the server refused.
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// JobLimits are the caps the worker enforces on a running job.
type JobLimits struct {
	WallMS   int64 `json:"wall_ms"`
	CPUMS    int64 `json:"cpu_ms"`
	MemBytes int64 `json:"mem_bytes"`
}

// Artifact is the pre-registered artifact a function job runs, at one exact version.
type Artifact struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// JobInput is one file the job's working directory must hold before the process starts: an OpenVibe.Media object
// pinned by digest. The worker fetches it with its own token and never a caller-chosen URL, and a size or digest
// mismatch ends the job failed before the process ever runs.
type JobInput struct {
	Name      string `json:"name"`
	MediaID   string `json:"media_id"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// Job is platform.job@1.
type Job struct {
	ID       string          `json:"id"` // job_<ULID>, minted by the dispatcher: the idempotency and ack key
	Class    string          `json:"class"`
	Artifact *Artifact       `json:"artifact,omitempty"`
	Args     json.RawMessage `json:"args"`
	TTLMS    int64           `json:"ttl_ms"`
	Limits   JobLimits       `json:"limits"`
	Net      string          `json:"net,omitempty"` // absent means deny
	Inputs   []JobInput      `json:"inputs,omitempty"`
}

// JobRequest is the `job` frame: run Job. A body that does not decode still yields the frame, with Err set and the
// id when it is a string, so the Node can nack it rather than drop it (the server resends a job until it is answered).
type JobRequest struct {
	Job Job   `json:"job"`
	Err error `json:"-"`
}

func (r *JobRequest) UnmarshalJSON(b []byte) error {
	var outer struct {
		Job json.RawMessage `json:"job"`
	}
	if err := json.Unmarshal(b, &outer); err != nil {
		return err
	}
	*r = JobRequest{}
	if len(outer.Job) == 0 {
		return nil
	}
	if err := json.Unmarshal(outer.Job, &r.Job); err != nil {
		var id struct {
			ID any `json:"id"`
		}
		_ = json.Unmarshal(outer.Job, &id)
		s, _ := id.ID.(string)
		r.Job, r.Err = Job{ID: s}, err
	}
	return nil
}

var (
	jobIDRe       = regexp.MustCompile(`^job_[0-9A-HJKMNP-TV-Z]{26}$`)
	artifactRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	artifactVerRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	mediaIDRe     = regexp.MustCompile(`^med_[0-9A-HJKMNP-TV-Z]{26}$`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	inputNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidJobID reports whether id is a job_<ULID>.
func ValidJobID(id string) bool { return jobIDRe.MatchString(id) }

// ValidMediaID reports whether id is a Media object id, med_<ULID>.
func ValidMediaID(id string) bool { return mediaIDRe.MatchString(id) }

// ValidJobInputs reports whether the job's inputs are well formed: at most maxJobInputs, each a Media object named by
// its contract's patterns, with names unique and carrying no path separator. An empty list is valid; nil is, too.
func ValidJobInputs(in []JobInput) bool {
	if len(in) > maxJobInputs {
		return false
	}
	seen := make(map[string]bool, len(in))
	for _, i := range in {
		if !inputNameRe.MatchString(i.Name) || !mediaIDRe.MatchString(i.MediaID) || !sha256Re.MatchString(i.SHA256) || i.SizeBytes < 0 {
			return false
		}
		if seen[i.Name] {
			return false
		}
		seen[i.Name] = true
	}
	return true
}

// maxJobInputs is the contract's cap on a job's inputs array.
const maxJobInputs = 32

// Check returns the fault code and reason to nack this job with, or two empty strings when a worker running the
// classes in available takes it. The checks run in this order, so the reason names the first problem.
func (r JobRequest) Check(available []string) (fault, reason string) {
	j := r.Job
	switch {
	case !ValidJobID(j.ID):
		return FaultBadValue, JobBadID
	case r.Err != nil:
		return FaultBadFrame, JobBadFrame
	case !contains(RuntimeClasses, j.Class):
		return FaultUnsupported, JobUnknownClass
	case (j.Class == ClassFunction || j.Class == ClassCode || j.Class == ClassLinux) && (j.Artifact == nil || !artifactRe.MatchString(j.Artifact.Name) || !artifactVerRe.MatchString(j.Artifact.Version)):
		return FaultBadValue, JobNoArtifact
	case j.Net != "" && j.Net != NetDeny && j.Net != NetNone && j.Net != NetPublic && j.Net != NetOpenVibeOnly:
		return FaultUnsupported, JobNetUnsupported
	case !ValidJobInputs(j.Inputs):
		return FaultBadValue, JobBadInputs
	case !isObject(j.Args) || j.TTLMS < 1 || j.Limits.WallMS < 1 || j.Limits.CPUMS < 1 || j.Limits.MemBytes < 1:
		return FaultBadValue, JobBadLimits
	case !contains(available, j.Class):
		return FaultUnsupported, JobNotAvailable
	}
	return "", ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func isObject(b json.RawMessage) bool {
	b = bytes.TrimSpace(b)
	return len(b) >= 2 && b[0] == '{'
}

// JobCancel stops a job; an unknown or ended id is ignored.
type JobCancel struct {
	ID string `json:"id"`
}

// JobExitAck tells the worker every usage reading of the job is recorded, so it stops resending job_exit.
type JobExitAck struct {
	ID string `json:"id"`
}

// ---- device → server ----

// JobStarted: the job's process started at StartedMS (worker wall clock, Unix ms), which anchors every usage second.
type JobStarted struct {
	ID        string `json:"id"`
	StartedMS int64  `json:"started_ms"`
}

// JobStdout is a chunk of the job's stdout. ChunkSeq is the job's own counter from 1 (the envelope owns seq).
type JobStdout struct {
	ID       string `json:"id"`
	ChunkSeq uint64 `json:"chunk_seq"`
	Chunk    string `json:"chunk"`
}

// JobUsage: wall-clock second Second (from 0 at StartedMS) of the job has fully elapsed. CPUMS is informational.
type JobUsage struct {
	ID        string `json:"id"`
	StartedMS int64  `json:"started_ms"`
	Second    int64  `json:"second"`
	CPUMS     *int64 `json:"cpu_ms,omitempty"`
}

// JobExitUsage is a job_exit's authoritative usage; StartedMS is set whenever WallMS > 0.
type JobExitUsage struct {
	StartedMS    *int64 `json:"started_ms,omitempty"`
	WallMS       int64  `json:"wall_ms"`
	CPUMS        *int64 `json:"cpu_ms,omitempty"`
	MemPeakBytes *int64 `json:"mem_peak_bytes,omitempty"`
}

// JobExit: the job ended. Code is null when the process was killed or never ran; Result is null unless it exited 0.
type JobExit struct {
	ID     string          `json:"id"`
	Reason string          `json:"reason"`
	Code   *int            `json:"code"`
	Result json.RawMessage `json:"result"`
	Usage  JobExitUsage    `json:"usage"`
}

// WorkerCapabilities is status.capabilities[CapWorker]: the runtime classes this Node runs and its local job caps.
type WorkerCapabilities struct {
	Capabilities   []string `json:"capabilities"` // the reserved Fabric names these classes advertise: worker:function, …
	RuntimeClasses []string `json:"runtime_classes"`
	MaxJobs        int      `json:"max_jobs"`
	MaxTTLMS       int64    `json:"max_ttl_ms"`
	MaxWallMS      int64    `json:"max_wall_ms"`
	MaxCPUMS       int64    `json:"max_cpu_ms"`
	MaxMemBytes    int64    `json:"max_mem_bytes"`
	MaxOutputBytes int64    `json:"max_output_bytes"`
}

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

// Heartbeat is sent every config.heartbeat_ms. Seq mirrors the envelope seq for older servers that echo it; T is the
// device's send time (unix ms), which Bot echoes back as heartbeat_ack.t/echo and the Node measures RTT from. RTTMS is
// the last measured round trip, absent until one has been measured (a measured 0 ms is still sent, hence the pointer).
type Heartbeat struct {
	Seq   uint64 `json:"seq"`
	T     int64  `json:"t,omitempty"`
	RTTMS *int64 `json:"rtt_ms,omitempty"`
}

// Reauth carries a fresh node token for the same principal; Bot closes the socket with 4002 when none arrives within
// 330 s of the last one. Only a Network-paired machine sends it.
type Reauth struct {
	Token string `json:"token"`
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
func (Reauth) MessageType() string       { return TypeReauth }
func (EstopState) MessageType() string   { return TypeEstopState }
func (JobRequest) MessageType() string   { return TypeJob }
func (JobCancel) MessageType() string    { return TypeJobCancel }
func (JobExitAck) MessageType() string   { return TypeJobExitAck }
func (JobStarted) MessageType() string   { return TypeJobStarted }
func (JobStdout) MessageType() string    { return TypeJobStdout }
func (JobUsage) MessageType() string     { return TypeJobUsage }
func (JobExit) MessageType() string      { return TypeJobExit }

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
	TypeReauth:       func() Message { return &Reauth{} },
	TypeEstopState:   func() Message { return &EstopState{} },
	TypeJob:          func() Message { return &JobRequest{} },
	TypeJobCancel:    func() Message { return &JobCancel{} },
	TypeJobExitAck:   func() Message { return &JobExitAck{} },
	TypeJobStarted:   func() Message { return &JobStarted{} },
	TypeJobStdout:    func() Message { return &JobStdout{} },
	TypeJobUsage:     func() Message { return &JobUsage{} },
	TypeJobExit:      func() Message { return &JobExit{} },
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
	case *Reauth:
		return *v
	case *EstopState:
		return *v
	case *JobRequest:
		return *v
	case *JobCancel:
		return *v
	case *JobExitAck:
		return *v
	case *JobStarted:
		return *v
	case *JobStdout:
		return *v
	case *JobUsage:
		return *v
	case *JobExit:
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
// publish key; null when Bot has no ingest configured) appear only here. POST /api/v1/devices/bind answers the same
// body without the credential.
type PairResponse struct {
	DeviceID   string          `json:"device_id"`
	Credential string          `json:"credential"`
	PublishKey string          `json:"publish_key"`
	WHIPURL    string          `json:"whip_url,omitempty"`
	RobotID    string          `json:"robot_id"`
	Profile    json.RawMessage `json:"profile,omitempty"`
}

// NodePairingRequest is the body of POST <network>/api/v1/node-pairing: the code is the credential. Pairing is the
// pair_… id from the installer command; with it a wrong try counts against that pairing's code.
type NodePairingRequest struct {
	Code    string `json:"code"`
	Pairing string `json:"pairing,omitempty"`
	Name    string `json:"name,omitempty"`
}

// PairedFor is the service and the reference there (Bot: the robot id) a node principal was paired for.
type PairedFor struct {
	Service string `json:"service"`
	Ref     string `json:"ref,omitempty"`
}

// NodePairingResponse is the 201 answer of POST /api/v1/node-pairing. The credential appears only here.
type NodePairingResponse struct {
	Principal     string     `json:"principal"` // nod_…
	NodeID        string     `json:"node_id"`
	HomeCell      string     `json:"home_cell"`
	Credential    string     `json:"credential"`
	TokenEndpoint string     `json:"token_endpoint,omitempty"`
	PairedFor     *PairedFor `json:"paired_for"`
}

// TokenResponse is the 200 answer of POST <network>/oauth/token.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"` // seconds (300 for a node token)
}

// OAuthError is the OAuth-shaped refusal of /oauth/token (invalid_client for a wrong or revoked credential).
type OAuthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// Problem is the RFC 9457 body of a non-2xx answer from Bot's REST API.
type Problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"` // e.g. bot.pairing_code_invalid
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
}
