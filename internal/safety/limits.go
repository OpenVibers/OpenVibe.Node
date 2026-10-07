package safety

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/OpenVibers/OpenVibe.Node/internal/protocol"
)

// Limits are the effective owner limits: the stricter of the server's config and the local config.
type Limits struct {
	MaxSpeed     float64 // 0..1, applied to throttle, x, y
	MaxTurn      float64 // 0..1, applied to steer, rotation
	MaxCommandMS int     // cap on deadline_ms
}

// DefaultLimits apply before the server sends config: full scale, deadlines capped at protocol.DefaultMaxCommandMS.
func DefaultLimits() Limits {
	return Limits{MaxSpeed: 1, MaxTurn: 1, MaxCommandMS: protocol.DefaultMaxCommandMS}
}

// Merge returns the stricter of the server limits and the local caps.
func Merge(server protocol.Limits, localSpeed, localTurn *float64, localMaxMS int) Limits {
	l := DefaultLimits()
	pick := func(cur float64, vals ...*float64) float64 {
		for _, v := range vals {
			if v != nil && !math.IsNaN(*v) {
				cur = math.Min(cur, math.Max(0, *v))
			}
		}
		return cur
	}
	l.MaxSpeed = pick(l.MaxSpeed, server.MaxSpeed, localSpeed)
	l.MaxTurn = pick(l.MaxTurn, server.MaxTurn, localTurn)
	if server.MaxCommandMS > 0 {
		l.MaxCommandMS = server.MaxCommandMS
	}
	if localMaxMS > 0 && localMaxMS < l.MaxCommandMS {
		l.MaxCommandMS = localMaxMS
	}
	return l
}

// Deadline returns the deadline in ms for a command: the default when absent, never above MaxCommandMS.
func (l Limits) Deadline(requested int) int {
	d := requested
	if d <= 0 {
		d = protocol.DefaultDeadlineMS
	}
	max := l.MaxCommandMS
	if max <= 0 {
		max = protocol.DefaultMaxCommandMS
	}
	if d > max {
		d = max
	}
	return d
}

var speedKeys = map[string]bool{"throttle": true, "x": true, "y": true}
var turnKeys = map[string]bool{"steer": true, "rotation": true}
var positionKeys = map[string]bool{"pan": true, "tilt": true, "zoom": true}

// FaultError is an error carrying a protocol fault code.
type FaultError struct{ Code, Message string }

func (e *FaultError) Error() string { return e.Code + ": " + e.Message }

func badValue(format string, a ...any) error {
	return &FaultError{Code: protocol.FaultBadValue, Message: fmt.Sprintf(format, a...)}
}

// Clamp checks a command value and clamps every motion field to the limits. It runs in the core, before a plugin
// sees the value, so no operator can exceed the owner's limits whatever the plugin does.
//
//	drive:    throttle, x, y → ±MaxSpeed; steer, rotation → ±MaxTurn (all must be numbers)
//	ptz:      pan, tilt, zoom → -1..1
//	actuator: a numeric "value" → -1..1; "name" must be a string
func (l Limits) Clamp(kind string, value json.RawMessage) (json.RawMessage, error) {
	v := map[string]any{}
	if len(bytes.TrimSpace(value)) > 0 && string(bytes.TrimSpace(value)) != "null" {
		d := json.NewDecoder(bytes.NewReader(value))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, badValue("value must be a JSON object")
		}
	}
	num := func(k string, raw any) (float64, error) {
		n, ok := raw.(json.Number)
		if !ok {
			return 0, badValue("%s must be a number", k)
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, badValue("%s must be a finite number", k)
		}
		return f, nil
	}
	clampTo := func(f, m float64) float64 { return math.Max(-m, math.Min(m, f)) }
	switch kind {
	case protocol.KindDrive:
		for k, raw := range v {
			switch {
			case speedKeys[k]:
				f, err := num(k, raw)
				if err != nil {
					return nil, err
				}
				v[k] = clampTo(clampTo(f, 1), l.MaxSpeed)
			case turnKeys[k]:
				f, err := num(k, raw)
				if err != nil {
					return nil, err
				}
				v[k] = clampTo(clampTo(f, 1), l.MaxTurn)
			}
		}
	case protocol.KindPTZ:
		for k, raw := range v {
			if positionKeys[k] {
				f, err := num(k, raw)
				if err != nil {
					return nil, err
				}
				v[k] = clampTo(f, 1)
			}
		}
	case protocol.KindActuator:
		name, ok := v["name"].(string)
		if !ok || name == "" {
			return nil, badValue("actuator needs a name")
		}
		if raw, ok := v["value"].(json.Number); ok {
			f, err := num("value", raw)
			if err != nil {
				return nil, err
			}
			v["value"] = clampTo(f, 1)
		}
	case protocol.KindSay:
		if s, ok := v["text"].(string); !ok || s == "" || len(s) > 1000 {
			return nil, badValue("say needs text (1 to 1000 bytes)")
		}
	case protocol.KindButton:
		name, ok := v["name"].(string)
		if !ok || name == "" {
			return nil, badValue("button needs a name")
		}
		if state, present := v["state"]; present && state != "down" && state != "up" {
			return nil, badValue("button state must be down or up")
		}
	case protocol.KindPoint:
		for _, key := range []string{"x", "y"} {
			f, err := num(key, v[key])
			if err != nil || f < 0 || f > 1 {
				return nil, badValue("point %s must be a number from 0 to 1", key)
			}
			v[key] = f
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, badValue("value could not be re-encoded")
	}
	return out, nil
}
