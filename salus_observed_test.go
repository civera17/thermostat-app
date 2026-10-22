package main

import (
	"encoding/json"
	"testing"
)

func TestIT700ScalesRemainIndependentAndExplicit(t *testing.T) {
	var sample struct {
		Evidence string
		Details  map[string]json.RawMessage
		History  map[string]struct{ Value json.RawMessage } `json:"latest_history"`
	}
	if err := json.Unmarshal(fixture(t, "salus-it700-scale.json"), &sample); err != nil {
		t.Fatal(err)
	}
	d := testDataset(t, "2026-10-15", "2026-10-15")
	const id = "SANITIZED-IT700"
	d.Config.SALUS.Temperatures[id] = map[string]TemperatureInterpretation{}
	for _, property := range []string{"current_temperature", "heating_setpoint"} {
		raw := sample.History[property].Value
		if d.temperature(id, property, raw) != nil {
			t.Fatal("numeric magnitude must not enable an interpretation implicitly")
		}
		d.Config.SALUS.Temperatures[id][property] = TemperatureInterpretation{1, sample.Evidence}
		got, want := d.temperature(id, property, raw), number(sample.Details[property])
		if got == nil || want == nil || *got != *want {
			t.Fatalf("%s history and details must agree at divisor 1", property)
		}
	}
}
