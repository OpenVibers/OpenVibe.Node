package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
