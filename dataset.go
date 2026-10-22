package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Device struct {
	Source   string                     `json:"source"`
	ID       string                     `json:"id"`
	Name     string                     `json:"name,omitempty"`
	Gateway  string                     `json:"gateway_id,omitempty"`
	Model    string                     `json:"model,omitempty"`
	Resource string                     `json:"resource_type,omitempty"`
	Unit     string                     `json:"unit,omitempty"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	WireID   json.RawMessage            `json:"-"`
}
type Window struct {
	Source string `json:"source"`
	Device string `json:"device_id,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Page   *int   `json:"page,omitempty"`
	Raw    string `json:"raw_file,omitempty"`
	Rows   int    `json:"rows"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
type Coverage struct {
	Device     string
	Start, End time.Time
	Complete   bool
}
type stateGap struct {
	Device, Property string
	Start, End       time.Time
}
type Consumption struct {
	Device                             Device
	Granularity, From, To, Date, PDate string
	Index                              *int
	Start, End                         time.Time
	TimeBasis                          string
	Raw                                map[string]json.RawMessage
	Value                              *float64
	KWh                                *float64
	Quality                            []string
}
type Event struct {
	Device              Device
	Property            string
	At                  time.Time
	RawAt               string
	Value, Old          json.RawMessage
	Quality             []string
	Duplicate, Conflict bool
}
type Manifest struct {
	SchemaVersion   int                          `json:"schema_version"`
	Status          string                       `json:"status"`
	Created         string                       `json:"created_at_utc"`
	Timezone        string                       `json:"timezone"`
	From            string                       `json:"from_inclusive"`
	To              string                       `json:"to_inclusive"`
	StartUTC        string                       `json:"start_utc"`
	EndUTC          string                       `json:"end_utc_exclusive"`
	Sources         []string                     `json:"sources"`
	Devices         []Device                     `json:"devices"`
	Windows         []Window                     `json:"windows"`
	Gaps            []string                     `json:"gaps"`
	Assumptions     []string                     `json:"interpretation_assumptions"`
	Alignment       string                       `json:"aligned_resolution"`
	Coverage        map[string]map[string]string `json:"actual_coverage"`
	InitialStates   map[string]map[string]string `json:"initial_states"`
	Interpretations map[string]any               `json:"configured_interpretations"`
	Context         HeatingContext               `json:"context,omitempty"`
}
type dataset struct {
	Dir         string
	Config      Config
	Range       DateRange
	Loc         *time.Location
	Scrub       *scrubber
	Manifest    Manifest
	Consumption []Consumption
	Events      []Event
	Coverage    []Coverage
	StateGaps   []stateGap
	rawCounter  int
	failures    int
}

func newDataset(dir string, c Config, r DateRange, loc *time.Location, s *scrubber) (*dataset, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, errors.New("cannot create output parent")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, errors.New("output directory must not already exist")
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0600); err != nil {
		return nil, err
	}
	d := &dataset{Dir: dir, Config: c, Range: r, Loc: loc, Scrub: s}
	d.Manifest = Manifest{SchemaVersion: 1, Status: "in_progress", Created: stamp(time.Now().UTC()), Timezone: loc.String(), From: r.Start.Format(time.DateOnly), To: r.End.AddDate(0, 0, -1).Format(time.DateOnly), StartUTC: stamp(r.Start.UTC()), EndUTC: stamp(r.End.UTC()), Devices: []Device{}, Windows: []Window{}, Gaps: []string{}, Coverage: map[string]map[string]string{}, Context: c.Context}
	d.Manifest.Assumptions = []string{
		"Requested dates are inclusive local calendar dates; internal bounds are [start,next local midnight).",
		"Balance ai_* is the app's consumption channel, not a client difference of aa_*. Live gas responses identify aa_day/aa_hour as block comparison averages; decreases are not counter resets. Other channel/cohort semantics remain unverified.",
		"Balance calendar dates use the app's UTC-derived pdate calendar label; configured-zone alignment is a declared assumption, not a verified meter timezone.",
		"Unverified Balance hourly indices have no fabricated timestamps and are excluded from alignment. Evidence-backed elapsed-local-hour mapping also requires exactly 23/24/25 rows for that local day.",
		"SALUS history is change events. Successful complete queries justify carrying reported states; unobserved changes or connectivity loss remain possible when connectivity history is unavailable.",
		"Temperature properties are converted separately only with device/property-specific documented divisor evidence; otherwise excluded from temperature metrics.",
		"Only explicit boolean running_state.heating/cooling is interpreted. Scalar state encodings remain unverified and are excluded from demand metrics.",
		"Demand duration is thermostat demand, not burner flame time or measured energy. Gas can include domestic hot water and cooking.",
		"No weather, occupancy or hot-water normalization is performed. Consumption changes alone do not establish efficiency improvements.",
	}
	d.Manifest.InitialStates = map[string]map[string]string{}
	d.Manifest.Interpretations = map[string]any{}
	if c.Balance != nil {
		d.Manifest.Interpretations["balance"] = map[string]any{"granularity": c.Balance.Granularity, "hourly_time_evidence": c.Balance.HourlyEvidence, "gas_kwh_per_m3": c.Balance.GasFactor, "gas_conversion_evidence": c.Balance.GasEvidence}
	}
	if c.SALUS != nil {
		d.Manifest.Interpretations["salus"] = map[string]any{"initial_lookback_days": c.SALUS.LookbackDays, "temperature_interpretations": c.SALUS.Temperatures}
	}
	if err := d.writeJSON("manifest.json", d.Manifest); err != nil {
		return nil, err
	}
	return d, nil
}
func (d *dataset) writeJSON(name string, v any) error {
	b, err := d.Scrub.json(v)
	if err != nil {
		return errors.New("cannot sanitize output JSON")
	}
	return os.WriteFile(filepath.Join(d.Dir, name), append(b, '\n'), 0600)
}
func (d *dataset) record(w Window, raw []byte, err error) error {
	if len(raw) > 0 && json.Valid(raw) {
		d.rawCounter++
		name := fmt.Sprintf("raw/%s-%06d.json", w.Source, d.rawCounter)
		if e := os.MkdirAll(filepath.Join(d.Dir, "raw"), 0700); e != nil {
			return e
		}
		if e := d.writeJSON(name, json.RawMessage(raw)); e != nil {
			return e
		}
		w.Raw = name
	}
	w.Status = "complete"
	if err != nil {
		w.Status = "failed"
		w.Error = err.Error()
		d.failures++
	}
	d.Manifest.Windows = append(d.Manifest.Windows, w)
	return nil
}
func (d *dataset) gap(s string) { d.Manifest.Gaps = append(d.Manifest.Gaps, s) }

func choose(devices []Device, ids []string) ([]Device, error) {
	if len(ids) == 0 {
		return devices, nil
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []Device
	for _, dev := range devices {
		if wanted[dev.ID] {
			out = append(out, dev)
			delete(wanted, dev.ID)
		}
	}
	if len(wanted) > 0 {
		return nil, errors.New("one or more configured device IDs were not returned by authorized discovery")
	}
	return out, nil
}

func (d *dataset) csv(name string, header []string, rows [][]string) error {
	f, err := os.OpenFile(filepath.Join(d.Dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err = w.Write(header); err == nil {
		for _, row := range rows {
			clean := make([]string, len(row))
			for i, s := range row {
				clean[i] = d.Scrub.cell(s)
			}
			if err = w.Write(clean); err != nil {
				break
			}
		}
	}
	w.Flush()
	if err == nil {
		err = w.Error()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func floatCell(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}
func boolCell(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
}
func jsonCell(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	return string(v)
}

func (d *dataset) prepare() {
	sort.SliceStable(d.Consumption, func(i, j int) bool {
		a, b := d.Consumption[i], d.Consumption[j]
		if a.Device.ID != b.Device.ID {
			return a.Device.ID < b.Device.ID
		}
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Index != nil && b.Index != nil {
			return *a.Index < *b.Index
		}
		return a.PDate < b.PDate
	})
	seen := map[string]int{}
	for i := range d.Consumption {
		c := &d.Consumption[i]
		key := c.Device.ID + "|" + c.Date
		if c.Index != nil {
			key += fmt.Sprint("|", *c.Index)
		}
		if c.Date != "" {
			if old, ok := seen[key]; ok {
				a, _ := json.Marshal(d.Consumption[old].Raw)
				b, _ := json.Marshal(c.Raw)
				if string(a) == string(b) {
					c.Quality = append(c.Quality, "duplicate")
					c.Value = nil
					c.KWh = nil
				} else {
					c.Quality = append(c.Quality, "conflicting_duplicate")
					c.Value = nil
					c.KWh = nil
					d.Consumption[old].Value = nil
					d.Consumption[old].KWh = nil
					d.Consumption[old].Quality = append(d.Consumption[old].Quality, "conflicting_duplicate")
				}
				d.gap("Balance duplicate row: " + key)
			} else {
				seen[key] = i
			}
		}
		if c.Value == nil {
			d.gap("Balance missing/invalid consumption: " + key)
		}
	}
	lastEvent := map[string]time.Time{}
	for i := range d.Events {
		e := &d.Events[i]
		key := e.Device.ID + "|" + e.Property
		if t, ok := lastEvent[key]; ok && e.At.Before(t) {
			e.Quality = append(e.Quality, "out_of_order_sorted")
			d.gap("SALUS out-of-order events sorted: " + key)
		}
		lastEvent[key] = e.At
		if e.Property == "running_state" {
			if heat, _ := runningState(e.Value); heat == nil {
				e.Quality = append(e.Quality, "state_encoding_unverified")
			}
		}
		if e.Property == "current_temperature" || e.Property == "heating_setpoint" {
			if d.temperature(e.Device.ID, e.Property, e.Value) == nil {
				e.Quality = append(e.Quality, "temperature_unverified_or_invalid")
			}
		}
	}
	sort.SliceStable(d.Events, func(i, j int) bool {
		a, b := d.Events[i], d.Events[j]
		if a.Device.ID != b.Device.ID {
			return a.Device.ID < b.Device.ID
		}
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.Property < b.Property
	})
	eventSeen := map[string]int{}
	for i := range d.Events {
		e := &d.Events[i]
		key := e.Device.ID + "|" + e.Property + "|" + stamp(e.At)
		if prev, ok := eventSeen[key]; ok {
			if string(e.Value) == string(d.Events[prev].Value) && string(e.Old) == string(d.Events[prev].Old) {
				e.Duplicate = true
				e.Quality = append(e.Quality, "duplicate")
			} else {
				e.Conflict = true
				d.Events[prev].Conflict = true
				e.Quality = append(e.Quality, "conflicting_same_timestamp")
				d.Events[prev].Quality = append(d.Events[prev].Quality, "conflicting_same_timestamp")
			}
			d.gap("SALUS duplicate/conflicting event: " + key)
		} else {
			eventSeen[key] = i
		}
	}
	previous := map[string]Event{}
	for i := range d.Events {
		e := &d.Events[i]
		if e.Duplicate {
			continue
		}
		key := e.Device.ID + "|" + e.Property
		if prev, ok := previous[key]; ok && e.At.After(prev.At) && !e.Conflict && !prev.Conflict && len(e.Old) > 0 && string(e.Old) != "null" && !sameValue(prev.Value, e.Old) {
			e.Quality = append(e.Quality, "old_value_disagrees_with_previous_event")
			d.StateGaps = append(d.StateGaps, stateGap{e.Device.ID, e.Property, prev.At, e.At})
			d.gap("SALUS continuity mismatch; state excluded between " + stamp(prev.At) + " and " + stamp(e.At) + ": " + key)
		}
		previous[key] = *e
	}
}

func sameValue(a, b json.RawMessage) bool {
	if scalar(a) == scalar(b) {
		return true
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	aa, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(aa) == string(bb)
}

func (d *dataset) finish() error {
	d.prepare()
	d.Manifest.Status = "complete"
	if d.failures > 0 {
		d.Manifest.Status = "partial"
	}
	var rows [][]string
	for _, c := range d.Consumption {
		index := ""
		if c.Index != nil {
			index = strconv.Itoa(*c.Index)
		}
		suffix := balanceSuffix(c.Granularity)
		rows = append(rows, []string{c.Device.ID, c.Device.Name, c.Device.Resource, c.Device.Unit, c.Granularity, c.From, c.To, c.Date, c.PDate, index, stamp(c.Start.UTC()), stamp(c.End.UTC()), stamp(c.Start), stamp(c.End), c.TimeBasis, jsonCell(c.Raw["ai_"+suffix]), jsonCell(c.Raw["fai_"+suffix]), floatCell(c.Value), jsonCell(c.Raw["aa_"+suffix]), jsonCell(c.Raw["bi_"+suffix]), jsonCell(c.Raw["fbi_"+suffix]), jsonCell(c.Raw["ba_"+suffix]), jsonCell(c.Raw["fba_"+suffix]), jsonCell(c.Raw["no_data"]), jsonCell(c.Raw["leak_type"]), floatCell(c.KWh), strings.Join(c.Quality, ";")})
	}
	if err := d.csv("consumption.csv", strings.Split("meter_id,meter_name,resource_type,unit,granularity,request_date00,request_date01,calendar_date,pdate_raw,hour_index,start_utc,end_utc,start_local,end_local,time_basis,ai_raw,fai_raw,consumption,aa_raw,bi_raw,fbi_raw,ba_raw,fba_raw,no_data_raw,leak_type_raw,gas_kwh,quality", ","), rows); err != nil {
		return err
	}
	rows = nil
	for _, e := range d.Events {
		temp := d.temperature(e.Device.ID, e.Property, e.Value)
		heat, cool := runningState(e.Value)
		if e.Property != "running_state" {
			heat = nil
			cool = nil
		}
		rows = append(rows, []string{e.Device.ID, e.Device.Name, e.Property, e.RawAt, stamp(e.At.UTC()), stamp(e.At.In(d.Loc)), strconv.FormatBool(d.Range.contains(e.At)), jsonCell(e.Value), jsonCell(e.Old), floatCell(number(e.Value)), floatCell(temp), boolCell(heat), boolCell(cool), strings.Join(e.Quality, ";")})
	}
	if err := d.csv("thermostat_events.csv", strings.Split("device_id,device_name,property,at_raw,at_utc,at_local,in_requested_range,value_raw_json,old_value_raw_json,value_numeric_raw,temperature_c,heating_demand,cooling_demand,quality", ","), rows); err != nil {
		return err
	}
	if err := d.align(); err != nil {
		return err
	}
	for _, dev := range d.Manifest.Devices {
		key := dev.Source + ":" + dev.ID
		minT, maxT := "", ""
		count := 0
		if dev.Source == "balance" {
			for _, c := range d.Consumption {
				if c.Device.ID == dev.ID {
					v := c.Date
					if v != "" && (minT == "" || v < minT) {
						minT = v
					}
					if v > maxT {
						maxT = v
					}
					count++
				}
			}
		} else {
			for _, e := range d.Events {
				if e.Device.ID == dev.ID && d.Range.contains(e.At) {
					v := stamp(e.At.UTC())
					if minT == "" || v < minT {
						minT = v
					}
					if v > maxT {
						maxT = v
					}
					count++
				}
			}
		}
		d.Manifest.Coverage[key] = map[string]string{"first_record": minT, "last_record": maxT, "native_rows": strconv.Itoa(count), "note": "Record extents do not establish continuous coverage; see windows, gaps and aligned known-seconds columns."}
	}
	if err := d.writeJSON("devices.json", d.Manifest.Devices); err != nil {
		return err
	}
	if err := d.writeJSON("context.json", d.Config.Context); err != nil {
		return err
	}
	if err := d.analysis(); err != nil {
		return err
	}
	return d.writeJSON("manifest.json", d.Manifest)
}
