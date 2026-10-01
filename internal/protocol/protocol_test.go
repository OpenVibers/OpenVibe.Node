package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func TestEncodeDecodeRoundTrip(t *testing.T) {
	latched := true
	msgs := []Message{
		Hello{DeviceID: "dev_1", RobotIDs: []string{"rob_1"}, ServerTime: "2026-09-29T19:20:00.000Z"},
		Config{Limits: Limits{MaxCommandMS: 500}, HeartbeatMS: 1000, AllowedCommands: []string{"drive"}, EstopLatched: &latched},
		Command{ID: "c1", Kind: KindDrive, Value: json.RawMessage(`{"throttle":0.5}`), DeadlineMS: 1738065600300,
			Operator: &Operator{Subject: "usr_1", Role: "operator"}},
		Estop{Latched: true, By: "usr_owner"},
		HeartbeatAck{ServerTime: "2026-09-29T19:20:00.501Z"},
		Error{Code: "bot.unknown_message"},
		Status{AgentVersion: "1", Drivers: []DriverStatus{}, Faults: []Fault{}},
		Telemetry{Sensors: map[string]any{"a": 1.0}},
		Ack{ID: "c1"},
		Nack{ID: "c1", FaultCode: FaultEstopped},
		Heartbeat{RTTMS: i64(7)},
		EstopState{Latched: true, By: "device"},
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
		if raw["v"] != float64(1) || raw["seq"] != float64(i+1) || raw["ts"] != float64(1234) || raw["type"] != m.MessageType() {
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

// TestBotFrames decodes frames exactly as OpenVibe.Bot's realtime.js (d398445) and docs/protocol.md write them.
func TestBotFrames(t *testing.T) {
	decode := func(s string) Message {
		t.Helper()
		f, err := Decode([]byte(s))
		if err != nil {
			t.Fatalf("%v: %s", err, s)
		}
		return f.Msg
	}
	h := decode(`{"v":1,"seq":1,"ts":1738065600000,"type":"hello","session_id":"sess_01J8Z4","device_id":"dev_01J8Z4","robot_ids":["rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X"],"server_time":"2026-09-29T19:20:00.000Z"}`).(Hello)
	if tm, ok := h.Time(); h.SessionID != "sess_01J8Z4" || len(h.RobotIDs) != 1 || !ok || tm.UnixMilli() != 1790709600000 {
		t.Fatalf("%+v", h)
	}
	c := decode(`{"v":1,"seq":2,"ts":1738065600001,"type":"config","heartbeat_ms":1000,"limits":{"max_speed":0.6,"max_command_ms":500},"allowed_commands":["drive","halt"],"estop_latched":true}`).(Config)
	if c.HeartbeatMS != 1000 || *c.Limits.MaxSpeed != 0.6 || c.Limits.MaxCommandMS != 500 || len(c.AllowedCommands) != 2 || c.EstopLatched == nil || !*c.EstopLatched {
		t.Fatalf("%+v", c)
	}
	if c := decode(`{"v":1,"seq":2,"ts":1,"type":"config","heartbeat_ms":1000,"limits":{},"allowed_commands":[],"estop_latched":false}`).(Config); c.AllowedCommands == nil || *c.EstopLatched {
		t.Fatalf("empty allowed_commands must mean none, not all: %+v", c)
	}
	cmd := decode(`{"v":1,"type":"command","seq":3,"ts":1738065600100,"id":"cmd_01J8Z5","ref":"op_7","kind":"drive","value":{"throttle":0.5,"steer":0},"deadline_ms":1738065600400,"operator":{"subject":"usr_01J8Z4","role":"operator"},"robot_id":"rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X"}`).(Command)
	if cmd.ID != "cmd_01J8Z5" || cmd.DeadlineMS != 1738065600400 || cmd.Operator == nil || cmd.Operator.Role != "operator" || cmd.RobotID == "" || string(cmd.Value) != `{"throttle":0.5,"steer":0}` {
		t.Fatalf("%+v", cmd)
	}
	if say := decode(`{"v":1,"type":"command","seq":4,"ts":1,"id":"cmd_2","ref":"op_8","kind":"say","value":{"text":"hi"},"deadline_ms":null,"operator":{"subject":"usr_1","role":"owner"},"robot_id":"rob_1"}`).(Command); say.DeadlineMS != 0 {
		t.Fatalf("%+v", say)
	}
	if e := decode(`{"v":1,"seq":5,"ts":1,"type":"estop","latched":false,"by":"usr_owner","at":"2026-09-29T19:20:01.200Z"}`).(Estop); e.Latched || e.By != "usr_owner" {
		t.Fatalf("%+v", e)
	}
	// Bot's heartbeat_ack carries the heartbeat's seq in the envelope's seq.
	f, err := Decode([]byte(`{"v":1,"seq":41,"ts":1738065600501,"type":"heartbeat_ack","server_time":"2026-09-29T19:20:00.501Z"}`))
	if err != nil || f.Seq != 41 || f.Msg.(HeartbeatAck).ServerTime == "" {
		t.Fatalf("%+v %v", f, err)
	}
	if e := decode(`{"v":1,"seq":6,"ts":1,"type":"error","code":"bot.unknown_message","detail":"unknown type x"}`).(Error); e.Code != "bot.unknown_message" || e.Detail == "" {
		t.Fatalf("%+v", e)
	}
	if _, err := Decode([]byte(`{"v":1,"seq":7,"ts":1,"type":"error","code":"bot.not_paired","detail":null}`)); err != nil {
		t.Fatal(err)
	}
}

// TestDeviceFrameFields checks the field names Bot's handleDeviceMessage reads.
func TestDeviceFrameFields(t *testing.T) {
	for _, c := range []struct {
		m    Message
		want []string
	}{
		{Heartbeat{RTTMS: i64(63)}, []string{`"rtt_ms":63`}},
		{Telemetry{Battery: f64(0.72), Voltage: f64(7.41)}, []string{`"battery":0.72`, `"voltage":7.41`}},
		{Status{Firmware: "openvibe-node-1", Capabilities: map[string]any{}, Faults: []Fault{}}, []string{`"firmware":"openvibe-node-1"`, `"capabilities":{}`, `"faults":[]`, `"estop_latched":false`}},
		{EstopState{Latched: true, By: "device", At: "2026-09-29T19:20:01.200Z"}, []string{`"latched":true`, `"by":"device"`, `"at":"2026-`}},
		{Nack{ID: "cmd_1", FaultCode: FaultExpired}, []string{`"id":"cmd_1"`, `"fault_code":"expired"`}},
	} {
		b, err := Encode(1, 1, c.m)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s lacks %s", b, w)
			}
		}
	}
	if b, _ := Encode(1, 1, Heartbeat{}); strings.Contains(string(b), "rtt_ms") {
		t.Errorf("rtt_ms before any measurement: %s", b)
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
