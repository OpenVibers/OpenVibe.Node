package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	msgs := []Message{
		Hello{SessionID: "sess_1", DeviceID: "dev_1", RobotIDs: []string{"rob_1"}},
		Config{Limits: Limits{MaxCommandMS: 500}, HeartbeatMS: 1000, AllowedCommands: []string{KindDrive}},
		Command{ID: "c1", Kind: KindDrive, Value: json.RawMessage(`{"throttle":0.5}`), DeadlineMS: 1738065600300},
		Estop{Latched: true, By: "usr_1"},
		HeartbeatAck{Seq: 5},
		Status{AgentVersion: "1", Drivers: []DriverStatus{}, Faults: []Fault{}},
		Telemetry{Sensors: map[string]any{"a": 1.0}},
		Ack{ID: "c1"},
		Nack{ID: "c1", FaultCode: FaultEstopped},
		Heartbeat{Seq: 10},
		EstopState{Latched: true},
		Error{Code: ErrNotPaired},
	}
	for i, m := range msgs {
		b, err := Encode(uint64(i+1), 1234, m)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("%s: %v: %s", m.MessageType(), err, b)
		}
		// heartbeat carries its own seq (mirroring the envelope's); heartbeat_ack does too only from older servers.
		_, ownSeq := m.(Heartbeat)
		if _, ok := m.(HeartbeatAck); ok {
			ownSeq = true
		}
		if raw["v"] != float64(1) || (!ownSeq && raw["seq"] != float64(i+1)) || raw["ts"] != float64(1234) || raw["type"] != m.MessageType() {
			t.Fatalf("envelope wrong: %s", b)
		}
		f, err := Decode(b)
		if err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		if f.Msg.MessageType() != m.MessageType() {
			t.Fatalf("type %s != %s", f.Msg.MessageType(), m.MessageType())
		}
	}
}

func TestCommandFields(t *testing.T) {
	f, err := Decode([]byte(`{"v":1,"seq":9,"ts":1,"type":"command","id":"x","ref":"op-1","kind":"drive","value":{"throttle":1},"deadline_ms":1738065600300,"operator":{"subject":"usr_1","role":"operator"},"robot_id":"rob_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := f.Msg.(Command)
	if c.ID != "x" || c.Ref != "op-1" || c.DeadlineMS != 1738065600300 || c.Operator == nil || c.Operator.Subject != "usr_1" || c.Operator.Role != "operator" ||
		c.RobotID != "rob_1" || string(c.Value) != `{"throttle":1}` {
		t.Fatalf("%+v", c)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bot", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBotFixtures decodes every frame Bot's docs/protocol.md shows for the device socket, copied verbatim into
// testdata/bot, and checks the fields the Node reads.
func TestBotFixtures(t *testing.T) {
	decode := func(name string) Frame {
		t.Helper()
		f, err := Decode(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return f
	}
	if h := decode("hello").Msg.(Hello); h.SessionID != "sess_01J8Z4…" || h.DeviceID != "dev_01J8Z4…" ||
		len(h.RobotIDs) != 1 || h.RobotIDs[0] != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X" || h.ServerTime != "2026-09-29T19:20:00.000Z" {
		t.Errorf("hello %+v", h)
	}
	if c := decode("config").Msg.(Config); c.HeartbeatMS != 1000 || c.Limits.MaxCommandMS != 300 || c.Limits.HeartbeatMS != 1000 ||
		c.Limits.MaxSpeed == nil || *c.Limits.MaxSpeed != 1 || len(c.AllowedCommands) != 3 || c.AllowedCommands[2] != KindHalt || c.EstopLatched {
		t.Errorf("config %+v", c)
	}
	f := decode("command_drive")
	if c := f.Msg.(Command); c.ID != "cmd_01J8Z4F…" || c.Kind != KindDrive || c.DeadlineMS-f.TS != 300 || c.Operator == nil ||
		c.Operator.Role != "operator" || c.RobotID != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X" || string(c.Value) != `{ "throttle": 0.6, "steer": -0.25 }` {
		t.Errorf("command %+v", c)
	}
	f = decode("command_actuator")
	if c := f.Msg.(Command); c.ID != "cmd_01J8Z4E…" || c.Kind != KindActuator || c.DeadlineMS-f.TS != 300 ||
		string(c.Value) != `{ "name": "pan", "value": -0.4 }` {
		t.Errorf("actuator command %+v", c)
	}
	if f := decode("heartbeat_ack"); f.Seq != 41 || f.Msg.(HeartbeatAck).Seq != 41 {
		t.Errorf("heartbeat_ack %+v", f)
	}
	// Bot's newer ack: its own envelope seq, the device's send time echoed as echo and t.
	if f := decode("heartbeat_ack_echo"); f.Seq != 4 || f.Msg.(HeartbeatAck).T != 1738065600500 ||
		f.Msg.(HeartbeatAck).Echo == nil || *f.Msg.(HeartbeatAck).Echo != 1738065600500 || f.Msg.(HeartbeatAck).ServerTime == "" {
		t.Errorf("heartbeat_ack echo %+v", f)
	}
	if e := decode("estop").Msg.(Estop); e.Latched || e.By != "usr_01J8…" {
		t.Errorf("estop %+v", e)
	}
	if e := decode("error").Msg.(Error); e.Code != "bot.forbidden" || e.Detail == "" {
		t.Errorf("error %+v", e)
	}
	// What the device sends decodes too, so the Node's types carry Bot's field names.
	if a := decode("ack").Msg.(Ack); a.ID != "cmd_01J8Z4F…" {
		t.Errorf("ack %+v", a)
	}
	if n := decode("nack").Msg.(Nack); n.FaultCode != "obstacle" {
		t.Errorf("nack %+v", n)
	}
	if h := decode("heartbeat").Msg.(Heartbeat); h.Seq != 41 || h.RTTMS == nil || *h.RTTMS != 63 {
		t.Errorf("heartbeat %+v", h)
	}
	if tl := decode("telemetry").Msg.(Telemetry); tl.Battery == nil || *tl.Battery != 0.72 || tl.Voltage == nil || tl.RSSI == nil {
		t.Errorf("telemetry %+v", tl)
	}
	if s := decode("estop_state").Msg.(EstopState); !s.Latched || s.By != "device" || s.At == "" {
		t.Errorf("estop_state %+v", s)
	}
	if s := decode("status").Msg.(Status); s.EstopLatched || s.Faults == nil {
		t.Errorf("status %+v", s)
	}
}

// TestBotPairShapes checks the pairing bodies against Bot's pair frame: the request carries exactly its fields, and
// the REST answer (v1.js: device_id, credential, publish_key, whip_url, robot_id = robot_ids[0], profile) decodes.
func TestBotPairShapes(t *testing.T) {
	var frame map[string]any
	if err := json.Unmarshal(fixture(t, "pair"), &frame); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "seq", "ts", "type"} {
		delete(frame, k)
	}
	b, _ := json.Marshal(PairRequest{Robot: "rob_1", Code: "7Q2M4XZP", AgentVersion: "0.1.0", DeviceKind: DeviceOnboard,
		Drivers: []string{}, Capabilities: map[string]any{}, Name: "Rover"})
	var ours map[string]any
	_ = json.Unmarshal(b, &ours)
	if len(ours) != len(frame) {
		t.Fatalf("request fields %v, Bot's pair frame has %v", ours, frame)
	}
	for k := range frame {
		if _, ok := ours[k]; !ok {
			t.Errorf("request lacks %q", k)
		}
	}
	var r PairResponse
	if err := json.Unmarshal(PairBodyFromPaired(fixture(t, "paired")), &r); err != nil {
		t.Fatal(err)
	}
	if r.DeviceID != "dev_01J8Z4…" || r.Credential != "Xb3…" || r.PublishKey != "Vt9…" || r.RobotID != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X" ||
		r.WHIPURL != "https://ingest.openre.stream/whip/Vt9…" || len(r.Profile) == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestDecodeRejects(t *testing.T) {
	if _, err := Decode([]byte(`{"v":2,"type":"hello"}`)); err == nil {
		t.Fatal("version 2 accepted")
	}
	if _, err := Decode([]byte(`nope`)); err == nil {
		t.Fatal("garbage accepted")
	}
	_, err := Decode([]byte(`{"v":1,"type":"future"}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

// TestJobFrameFixtures decodes every valid platform.job-frame@1 fixture from OpenVibe.Contracts (testdata/contracts),
// checks the fields the Node reads, and encodes it back to the same JSON object.
func TestJobFrameFixtures(t *testing.T) {
	const id = "job_01JAB2C3D4E5F6G7H8J9K0MNPQ"
	for _, name := range []string{"job", "job-cancel", "job-exit-ack", "job-started", "job-stdout", "job-usage", "job-exit", "job-exit-never-started"} {
		b, err := os.ReadFile(filepath.Join("testdata", "contracts", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := Decode(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch m := f.Msg.(type) {
		case JobRequest:
			j := m.Job
			if m.Err != nil || j.ID != id || j.Class != ClassFunction || j.Artifact == nil || j.Artifact.Name != "thumbnail" ||
				j.Artifact.Version != "1.2.0" || j.TTLMS != 60000 || j.Limits != (JobLimits{WallMS: 30000, CPUMS: 30000, MemBytes: 268435456}) || j.Net != "" {
				t.Errorf("%s: %+v", name, m)
			}
			if fault, reason := m.Check([]string{ClassFunction}); fault != "" {
				t.Errorf("%s: the contract's example is refused: %s %s", name, fault, reason)
			}
		case JobCancel:
			if m.ID != id {
				t.Errorf("%s: %+v", name, m)
			}
		case JobExitAck:
			if m.ID != id {
				t.Errorf("%s: %+v", name, m)
			}
		case JobStarted:
			if m.ID != id || m.StartedMS != 1790935200250 {
				t.Errorf("%s: %+v", name, m)
			}
		case JobStdout:
			if m.ID != id || m.ChunkSeq != 1 || f.Seq != 13 || m.Chunk != "resized 1 of 1\n" {
				t.Errorf("%s: %+v (envelope seq %d)", name, m, f.Seq)
			}
		case JobUsage:
			if m.ID != id || m.StartedMS != 1790935200250 || m.Second != 0 || m.CPUMS == nil || *m.CPUMS != 640 {
				t.Errorf("%s: %+v", name, m)
			}
		case JobExit:
			ran := name == "job-exit"
			if m.ID != id || (ran && (m.Reason != ExitExited || m.Code == nil || *m.Code != 0 || m.Usage.StartedMS == nil || m.Usage.WallMS != 2350)) ||
				(!ran && (m.Reason != ExitTTL || m.Code != nil || string(m.Result) != "null" || m.Usage.StartedMS != nil || m.Usage.WallMS != 0)) {
				t.Errorf("%s: %+v", name, m)
			}
		default:
			t.Fatalf("%s: decoded as %T", name, f.Msg)
		}
		out, err := Encode(f.Seq, f.TS, f.Msg)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		_ = json.Unmarshal(b, &want)
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s does not round-trip:\n want %v\n  got %v", name, want, got)
		}
	}
}

func TestWorkerCapabilityNames(t *testing.T) {
	for _, tc := range []struct {
		classes []string
		want    []string
	}{
		{nil, []string{}},
		{[]string{ClassFunction}, []string{"worker:function"}},
		{[]string{ClassFunction, ClassCode}, []string{"worker:function", "worker:code"}},
	} {
		if got := WorkerCapabilityNames(tc.classes); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("WorkerCapabilityNames(%v) = %v, want %v", tc.classes, got, tc.want)
		}
	}
}

func TestJobCheck(t *testing.T) {
	const id = "job_01JAB2C3D4E5F6G7H8J9K0MNPQ"
	valid := func() Job {
		return Job{ID: id, Class: ClassFunction, Artifact: &Artifact{Name: "thumbnail", Version: "1.2.0"}, Args: json.RawMessage(`{}`),
			TTLMS: 1000, Limits: JobLimits{WallMS: 1, CPUMS: 1, MemBytes: 1}}
	}
	cases := []struct {
		name      string
		edit      func(*Job)
		available []string
		reason    string
	}{
		{"taken", func(*Job) {}, []string{ClassFunction}, ""},
		{"net deny taken", func(j *Job) { j.Net = NetDeny }, []string{ClassFunction}, ""},
		{"nothing advertised", func(*Job) {}, nil, JobNotAvailable},
		{"other class advertised", func(*Job) {}, []string{ClassCode}, JobNotAvailable},
		{"code needs no artifact", func(j *Job) { j.Class, j.Artifact = ClassCode, nil }, nil, JobNotAvailable},
		{"no id", func(j *Job) { j.ID = "" }, []string{ClassFunction}, JobBadID},
		{"id without prefix", func(j *Job) { j.ID = "01JAB2C3D4E5F6G7H8J9K0MNPQ" }, []string{ClassFunction}, JobBadID},
		{"id with lower case", func(j *Job) { j.ID = "job_01jab2c3d4e5f6g7h8j9k0mnpq" }, []string{ClassFunction}, JobBadID},
		{"unknown class", func(j *Job) { j.Class = "quantum" }, []string{ClassFunction}, JobUnknownClass},
		{"no class", func(j *Job) { j.Class = "" }, []string{ClassFunction}, JobUnknownClass},
		{"no artifact", func(j *Job) { j.Artifact = nil }, []string{ClassFunction}, JobNoArtifact},
		{"no version", func(j *Job) { j.Artifact.Version = "" }, []string{ClassFunction}, JobNoArtifact},
		{"version range", func(j *Job) { j.Artifact.Version = "^1.2" }, []string{ClassFunction}, JobNoArtifact},
		{"version span", func(j *Job) { j.Artifact.Version = "1.2 - 2" }, []string{ClassFunction}, JobNoArtifact},
		{"bad artifact name", func(j *Job) { j.Artifact.Name = "Thumbnail" }, []string{ClassFunction}, JobNoArtifact},
		{"net allow", func(j *Job) { j.Net = "allow" }, []string{ClassFunction}, JobNetUnsupported},
		{"no args", func(j *Job) { j.Args = nil }, []string{ClassFunction}, JobBadLimits},
		{"args not an object", func(j *Job) { j.Args = json.RawMessage(`[1]`) }, []string{ClassFunction}, JobBadLimits},
		{"no ttl", func(j *Job) { j.TTLMS = 0 }, []string{ClassFunction}, JobBadLimits},
		{"no mem limit", func(j *Job) { j.Limits.MemBytes = 0 }, []string{ClassFunction}, JobBadLimits},
	}
	for _, c := range cases {
		j := valid()
		c.edit(&j)
		if _, reason := (JobRequest{Job: j}).Check(c.available); reason != c.reason {
			t.Errorf("%s: reason %q, want %q", c.name, reason, c.reason)
		}
	}
}

// TestJobUndecodableBody: a job body of the wrong shape still decodes as a job frame, with the id and Err, so the
// Node nacks it instead of dropping it.
func TestJobUndecodableBody(t *testing.T) {
	f, err := Decode([]byte(`{"v":1,"seq":1,"ts":1,"type":"job","job":{"id":"job_01JAB2C3D4E5F6G7H8J9K0MNPQ","class":"function","ttl_ms":"soon"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := f.Msg.(JobRequest)
	if r.Err == nil || r.Job.ID != "job_01JAB2C3D4E5F6G7H8J9K0MNPQ" {
		t.Fatalf("%+v", r)
	}
	if fault, reason := r.Check([]string{ClassFunction}); fault != FaultBadFrame || reason != JobBadFrame {
		t.Fatalf("%s %s", fault, reason)
	}
	f, err = Decode([]byte(`{"v":1,"seq":1,"ts":1,"type":"job","job":{"id":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := f.Msg.(JobRequest).Check(nil); reason != JobBadID {
		t.Fatalf("numeric id: %q", reason)
	}
}
