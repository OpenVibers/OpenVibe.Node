package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	msgs := []Message{
		Hello{DeviceID: "dev_1"},
		Config{Limits: Limits{MaxCommandMS: 500}, HeartbeatMS: 1000},
		Command{ID: "c1", Kind: KindDrive, Value: json.RawMessage(`{"throttle":0.5}`), DeadlineMS: 300},
		Estop{Reason: "owner"},
		EstopClear{By: "owner"},
		HeartbeatAck{T: 5},
		Status{AgentVersion: "1", Drivers: []DriverStatus{}, Faults: []Fault{}},
		Telemetry{Sensors: map[string]any{"a": 1.0}},
		Ack{ID: "c1"},
		Nack{ID: "c1", FaultCode: FaultEstopped},
		Heartbeat{T: 7},
		EstopState{Latched: true},
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

func TestCommandFields(t *testing.T) {
	f, err := Decode([]byte(`{"v":1,"seq":9,"ts":1,"type":"command","id":"x","kind":"drive","value":{"throttle":1},"deadline_ms":250,"operator":"usr_1","role":"operator"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := f.Msg.(Command)
	if c.ID != "x" || c.DeadlineMS != 250 || c.Operator != "usr_1" || string(c.Value) != `{"throttle":1}` {
		t.Fatalf("%+v", c)
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
