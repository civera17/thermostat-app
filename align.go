package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type segment struct {
	Start, End   time.Time
	Temp, Target *float64
	Heat         *bool
	Coverage     string
}
type weighted struct{ TempSum, TempSeconds, TargetSum, TargetSeconds, HeatSeconds, HeatKnown, DeltaSum, JointSeconds, Over, Under float64 }

func (w *weighted) add(s segment, from, to time.Time) {
	a, b := maxTime(s.Start, from), minTime(s.End, to)
	if !a.Before(b) {
		return
	}
	seconds := b.Sub(a).Seconds()
	if s.Temp != nil {
		w.TempSum += *s.Temp * seconds
		w.TempSeconds += seconds
	}
	if s.Target != nil {
		w.TargetSum += *s.Target * seconds
		w.TargetSeconds += seconds
	}
	if s.Heat != nil {
		w.HeatKnown += seconds
		if *s.Heat {
			w.HeatSeconds += seconds
		}
	}
	if s.Temp != nil && s.Target != nil {
		delta := *s.Temp - *s.Target
		w.DeltaSum += delta * seconds
		w.JointSeconds += seconds
		if delta > 0 {
			w.Over += delta * seconds / 3600
		} else {
			w.Under -= delta * seconds / 3600
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func mean(sum, seconds float64) string {
	if seconds == 0 {
		return ""
	}
	v := sum / seconds
	return floatCell(&v)
}
func numeric(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func (d *dataset) covered(id string, at time.Time) bool {
	for _, c := range d.Coverage {
		if c.Device == id && !at.Before(c.Start) && at.Before(c.End) {
			return c.Complete
		}
	}
	return false
}
func (d *dataset) coveredRange(id string, from, to time.Time) bool {
	if from.Equal(to) {
		return d.covered(id, from)
	}
	seconds := 0.0
	for _, c := range d.Coverage {
		if c.Device == id && c.Complete {
			a, b := maxTime(from, c.Start), minTime(to, c.End)
			if a.Before(b) {
				seconds += b.Sub(a).Seconds()
			}
		}
	}
	return seconds == to.Sub(from).Seconds()
}
func (d *dataset) segments(dev Device) []segment {
	var events []Event
	start := d.Range.Start
	bounds := []time.Time{d.Range.Start, d.Range.End}
	for _, c := range d.Coverage {
		if c.Device == dev.ID {
			start = minTime(start, c.Start)
			bounds = append(bounds, c.Start, c.End)
		}
	}
	for _, e := range d.Events {
		if e.Device.ID == dev.ID && !e.Duplicate {
			events = append(events, e)
			bounds = append(bounds, e.At)
		}
	}
	state := map[string]json.RawMessage{}
	seeded := map[string]bool{}
	for _, e := range events {
		if !seeded[e.Property] {
			seeded[e.Property] = true
			if !e.Conflict && len(e.Old) > 0 && string(e.Old) != "null" && d.coveredRange(dev.ID, start, e.At) {
				state[e.Property] = e.Old
			}
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })
	times := bounds[:0]
	for _, t := range bounds {
		if len(times) == 0 || !t.Equal(times[len(times)-1]) {
			times = append(times, t)
		}
	}
	var props []string
	_ = json.Unmarshal(dev.Metadata["history_properties"], &props)
	hasConnection := false
	for _, e := range events {
		if e.Property == "connected" || e.Property == "online_status" {
			hasConnection = true
		}
	}
	for _, p := range props {
		if p == "online_status" || p == "connected" {
			hasConnection = true
		}
	}
	index := 0
	previousCovered := true
	var out []segment
	for i := 0; i+1 < len(times); i++ {
		a, b := times[i], times[i+1]
		if a.Before(start) || !a.Before(d.Range.End) {
			continue
		}
		covered := d.covered(dev.ID, a)
		if !covered || !previousCovered {
			state = map[string]json.RawMessage{}
		}
		previousCovered = covered
		for index < len(events) && events[index].At.Before(a) {
			index++
		}
		for index < len(events) && events[index].At.Equal(a) {
			e := events[index]
			index++
			if !covered {
				continue
			}
			if e.Conflict {
				delete(state, e.Property)
				continue
			}
			if e.Property != "connected" && e.Property != "online_status" {
				offline := false
				for _, key := range []string{"connected", "online_status"} {
					if online := boolValue(state[key]); online != nil && !*online {
						offline = true
					}
				}
				if offline {
					continue
				}
			}
			state[e.Property] = e.Value
			if e.Property == "connected" || e.Property == "online_status" {
				if online := boolValue(e.Value); online != nil && !*online {
					delete(state, "current_temperature")
					delete(state, "heating_setpoint")
					delete(state, "running_state")
				}
			}
		}
		s := segment{Start: a, End: b, Coverage: "missing_query_coverage"}
		if covered {
			online := true
			connection := "event_history_connectivity_unobserved"
			if hasConnection {
				connection = "connection_unknown"
				online = false
				for _, key := range []string{"connected", "online_status"} {
					if value := boolValue(state[key]); value != nil {
						online = *value
						if online {
							connection = "connected_event_state"
						} else {
							connection = "disconnected"
						}
						break
					}
				}
			}
			s.Coverage = connection
			if online {
				s.Temp = d.temperature(dev.ID, "current_temperature", state["current_temperature"])
				s.Target = d.temperature(dev.ID, "heating_setpoint", state["heating_setpoint"])
				s.Heat, _ = runningState(state["running_state"])
			}
		}
		for _, gap := range d.StateGaps {
			if gap.Device == dev.ID && !a.Before(gap.Start) && a.Before(gap.End) {
				switch gap.Property {
				case "current_temperature":
					s.Temp = nil
				case "heating_setpoint":
					s.Target = nil
				case "running_state":
					s.Heat = nil
				case "connected", "online_status":
					s.Temp = nil
					s.Target = nil
					s.Heat = nil
					s.Coverage = "connectivity_continuity_gap"
				}
			}
		}
		if a.Equal(d.Range.Start) {
			initial := map[string]string{"current_temperature": "unknown_or_unverified", "heating_setpoint": "unknown_or_unverified", "running_state": "unknown_or_unverified", "connectivity": s.Coverage}
			if s.Temp != nil {
				initial["current_temperature"] = "known_from_history"
			}
			if s.Target != nil {
				initial["heating_setpoint"] = "known_from_history"
			}
			if s.Heat != nil {
				initial["running_state"] = "known_from_history"
			}
			d.Manifest.InitialStates[dev.ID] = initial
			for p, state := range initial {
				if state == "unknown_or_unverified" {
					d.gap("SALUS initial state unknown/unverified: " + dev.ID + " " + p)
				}
			}
		}
		if d.Range.overlaps(a, b) {
			s.Start = maxTime(s.Start, d.Range.Start)
			s.End = minTime(s.End, d.Range.End)
			out = append(out, s)
		}
	}
	return out
}
func (d *dataset) binEnd(start time.Time, resolution string) time.Time {
	switch resolution {
	case "hourly":
		return minTime(start.Add(time.Hour), d.Range.End)
	case "monthly":
		return minTime(time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, d.Loc), d.Range.End)
	default:
		return minTime(start.AddDate(0, 0, 1), d.Range.End)
	}
}
func (d *dataset) consumptionBin(dev Device, from, to time.Time) (*float64, float64, *float64) {
	sum, kwh, seconds := 0.0, 0.0, 0.0
	kwhKnown := true
	last := from
	for _, c := range d.Consumption {
		if c.Device.ID != dev.ID || c.Start.IsZero() || c.Start.Before(from) || c.End.After(to) {
			continue
		}
		if c.Value == nil {
			continue
		}
		if c.Start.Before(last) {
			return nil, seconds, nil
		}
		last = c.End
		sum += *c.Value
		seconds += c.End.Sub(c.Start).Seconds()
		if c.KWh == nil {
			kwhKnown = false
		} else {
			kwh += *c.KWh
		}
	}
	if seconds != to.Sub(from).Seconds() {
		return nil, seconds, nil
	}
	if kwhKnown {
		return &sum, seconds, &kwh
	}
	return &sum, seconds, nil
}
func (d *dataset) align() error {
	resolution := "hourly"
	if b := d.Config.Balance; b != nil {
		resolution = b.Granularity
		if resolution == "hourly" && b.HourlyEvidence == "" {
			resolution = "daily"
		}
	}
	d.Manifest.Alignment = resolution
	var meters, thermostats []Device
	segments := map[string][]segment{}
	for _, dev := range d.Manifest.Devices {
		if dev.Source == "balance" {
			meters = append(meters, dev)
		} else {
			thermostats = append(thermostats, dev)
			segments[dev.ID] = d.segments(dev)
		}
	}
	if len(meters) == 0 {
		meters = []Device{{}}
	}
	if len(thermostats) == 0 {
		thermostats = []Device{{}}
	}
	var rows [][]string
	for cur := d.Range.Start; cur.Before(d.Range.End); {
		end := d.binEnd(cur, resolution)
		for _, meter := range meters {
			value, consSeconds, kwh := d.consumptionBin(meter, cur, end)
			for _, dev := range thermostats {
				w := weighted{}
				coverage := map[string]bool{}
				for _, s := range segments[dev.ID] {
					if s.Start.Before(end) && s.End.After(cur) {
						w.add(s, cur, end)
						coverage[s.Coverage] = true
					}
				}
				labels := make([]string, 0, len(coverage))
				for c := range coverage {
					labels = append(labels, c)
				}
				sort.Strings(labels)
				heat, over, under := "", "", ""
				if w.HeatKnown > 0 {
					heat = numeric(w.HeatSeconds)
				}
				if w.JointSeconds > 0 {
					over = numeric(w.Over)
					under = numeric(w.Under)
				}
				rows = append(rows, []string{stamp(cur.UTC()), stamp(end.UTC()), stamp(cur), stamp(end), resolution, numeric(end.Sub(cur).Seconds()), meter.ID, meter.Unit, floatCell(value), numeric(consSeconds), floatCell(kwh), dev.ID, mean(w.TempSum, w.TempSeconds), numeric(w.TempSeconds), mean(w.TargetSum, w.TargetSeconds), numeric(w.TargetSeconds), mean(w.DeltaSum, w.JointSeconds), numeric(w.JointSeconds), over, under, heat, numeric(w.HeatKnown), strings.Join(labels, ";")})
			}
		}
		cur = end
	}
	return d.csv("analysis_aligned.csv", strings.Split("start_utc,end_utc,start_local,end_local,resolution,interval_seconds,meter_id,consumption_unit,consumption,consumption_known_seconds,gas_kwh,thermostat_id,room_temperature_mean_c,temperature_known_seconds,setpoint_mean_c,setpoint_known_seconds,temperature_minus_target_mean_c,temp_target_overlap_seconds,overshoot_degree_hours,undershoot_degree_hours,heating_demand_seconds,heating_state_known_seconds,thermostat_coverage_basis", ","), rows)
}

func (d *dataset) analysis() error {
	valid := 0
	for _, c := range d.Consumption {
		if c.Value != nil {
			valid++
		}
	}
	content := fmt.Sprintf(`# Heating dataset

Export status: **%s**. Timezone: %s. Requested %s through %s, both dates inclusive.

Native consumption rows: %d (%d valid values). Thermostat events retained including lookback: %d.
Failed requests/windows: %d. Data-quality notes/gaps: %d. Aligned resolution: %s.

## Files and supported metrics

- consumption.csv retains native interval values, flags and aa_*. Live gas responses identify aa_day/aa_hour as block comparison averages; decreases are not counter resets. No cumulative-counter differencing or daily-to-hourly allocation is performed.
- thermostat_events.csv retains change events and old values, including preceding events marked outside the requested range. Raw values are JSON cells; numeric raw values are not automatically degrees Celsius.
- analysis_aligned.csv contains interval consumption only when a whole bin has valid, nonoverlapping consumption coverage. Blank means unavailable, never zero. Daily rows support consumption-per-day comparisons; finer rows can be grouped only when their coverage is complete.
- Temperature and setpoint means are time weighted. Their known-seconds columns are the actual denominator. Temperature-minus-target and overshoot/undershoot degree-hours require simultaneous verified temperature and target coverage. They are not extrapolated to uncovered time.
- Heating-demand seconds use explicit boolean running-state objects. This is thermostat demand, **not burner flame duration or measured heat/energy**. Unknown demand intervals remain excluded; heating_state_known_seconds is the denominator.
- Each aligned row is one meter/thermostat/bin pair. Multiple thermostats repeat the same meter consumption: never sum that consumption across thermostat pairs.
- manifest.json records every requested window/page, raw path, failure, data-quality gap, device and coverage extent. context.json contains only supplied context. Raw files are sanitized final responses, never authentication responses; redactions mean they are not byte-identical captures.

## Coverage and interpretation

--to is inclusive; internal time bounds are start local midnight through the following local midnight, exclusive. Calendar stepping handles DST; local timestamps include offsets. SALUS requests use UTC instants and response events are clipped to half-open bounds regardless of endpoint boundary inclusivity.

Balance hourly ordering, server timezone, gaps and DST handling were not proven by the app's synthetic index labels. Unverified indices have blank timestamps and are excluded from aligned calculations. If explicit hourly evidence is configured, elapsed-hour mapping additionally requires the exact local-day row count. Daily/monthly Balance pdate is a calendar label; interpreting that label in the configured timezone remains an explicit assumption. Monthly totals overlapping a partial requested month are retained natively but not prorated into alignment.

Temperature history conversion requires separate evidence for each device/property. Device-detail scaling does not establish history scaling; no magnitude-based guessing is used. A missing divisor/evidence leaves temperature metrics blank. Scalar running-state encodings remain uninterpreted.

SALUS carries reported values only inside complete API-query coverage. Bounded preceding history and the earliest event's old_value can establish initial state when continuous queried coverage supports it; otherwise state starts unknown. Failed query windows reset state. Explicit disconnection clears state; reconnecting does not restore stale pre-disconnection values. When historical connectivity is not supported, complete change history supports an unchanged *reported* state, but actual connectivity/staleness is unobserved; the CSV labels this assumption. No current snapshot is backdated into history.

History retention is typically about three months according to the SALUS specification. A successful empty query does not prove the device was unchanged or that older history exists. See raw pages, initial-state/known-seconds coverage, and manifest gaps rather than assuming continuous telemetry.

## Analysis guidance

Compare consumption per complete day, verified room temperature versus target, overshoot/undershoot, and thermostat-demand duration using their coverage denominators. Preserve raw flags, missingness, duplicates, conflicts and comparison averages. ai_* is the app's consumption chart channel; observed gas aa_day/aa_hour values reproduce block-comparison benchmarks. Their cohort/normalization rules remain unverified and they are not cumulative counters. Gas remains in native units; gas_kwh is populated only with a supplied conversion factor and documented source. Gas may include hot water and cooking.

Causal efficiency comparisons require comparable outdoor weather, occupancy, domestic hot water and heating settings. A change in consumption alone is **not evidence of improved efficiency**. No outdoor weather, occupancy or hot-water model is included. Do not optimize away comfort or infer boiler combustion efficiency from thermostat demand.

For unresolved evidence, see docs/INTERPRETATIONS.md in the CLI source. Do not upload configuration credentials. Raw/device data and context can still contain private household information; review the dataset before sharing it.
`, d.Manifest.Status, d.Loc.String(), d.Manifest.From, d.Manifest.To, len(d.Consumption), valid, len(d.Events), d.failures, len(d.Manifest.Gaps), d.Manifest.Alignment)
	return os.WriteFile(filepath.Join(d.Dir, "analysis.md"), []byte(d.Scrub.text(content)), 0600)
}
