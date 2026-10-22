package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed docs/salus-history-models.json
var salusModelJSON []byte
var salusProperties = func() map[string][]string {
	var m map[string][]string
	if err := json.Unmarshal(salusModelJSON, &m); err != nil {
		panic(err)
	}
	return m
}()

type salusClient struct {
	cfg        SALUSConfig
	wire       *wire
	scrub      *scrubber
	token      string
	authFailed bool
}

func (s *salusClient) login(ctx context.Context) error {
	raw, status, err := s.wire.request(ctx, "POST", "/login", "", map[string]string{"email": s.cfg.Email, "password": s.cfg.Password})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		if status >= 400 && status < 500 && status != 429 {
			s.authFailed = true
		}
		return &apiError{status, "SALUS_LOGIN_FAILED"}
	}
	var auth struct {
		Token string `json:"access_token"`
		Type  string `json:"token_type"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Token == "" || !strings.EqualFold(auth.Type, "Bearer") {
		s.authFailed = true
		return errors.New("SALUS login returned no Bearer token")
	}
	s.token = auth.Token
	s.scrub.add(s.token)
	return nil
}
func (s *salusClient) get(ctx context.Context, path string) ([]byte, error) {
	if s.authFailed {
		return nil, errors.New("SALUS session unavailable after authentication rejection")
	}
	for attempt := 0; attempt < 2; attempt++ {
		raw, status, err := s.wire.request(ctx, "GET", path, s.token, nil)
		if err != nil {
			return raw, err
		}
		if status == 401 && attempt == 0 {
			if err = s.login(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if status < 200 || status >= 300 {
			return raw, &apiError{status, "SALUS_READ_FAILED"}
		}
		return raw, nil
	}
	return nil, errors.New("SALUS session renewal failed")
}
func (s *salusClient) discover(ctx context.Context, d *dataset) ([]Device, error) {
	record := func(label string, raw []byte, err error) error {
		if d != nil {
			return d.record(Window{Source: "salus", Device: label}, raw, err)
		}
		return nil
	}
	raw, err := s.get(ctx, "/premium-lite/gateways")
	if e := record("gateways", raw, err); e != nil {
		return nil, e
	}
	if err != nil {
		return nil, err
	}
	var gateways []struct {
		Code string `json:"device_code"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &gateways) != nil || string(raw) == "null" {
		return nil, errors.New("SALUS gateways response is not an array")
	}
	var devices []Device
	seen := map[string]bool{}
	for _, g := range gateways {
		if g.Code == "" {
			return nil, errors.New("SALUS gateway has no device_code")
		}
		raw, err = s.get(ctx, "/premium-lite/gateways/"+url.PathEscape(g.Code)+"/devices")
		if e := record(g.Code+":devices", raw, err); e != nil {
			return nil, e
		}
		if err != nil {
			return nil, err
		}
		var codes []string
		if json.Unmarshal(raw, &codes) != nil || string(raw) == "null" {
			return nil, errors.New("SALUS device list is not an array")
		}
		for _, code := range codes {
			if code == "" {
				return nil, errors.New("SALUS device has empty ID")
			}
			if seen[code] {
				continue
			}
			seen[code] = true
			raw, err = s.get(ctx, "/premium-lite/devices/"+url.PathEscape(code))
			if e := record(code+":details", raw, err); e != nil {
				return nil, e
			}
			if err != nil {
				return nil, err
			}
			var details map[string]json.RawMessage
			if json.Unmarshal(raw, &details) != nil || scalar(details["device_code"]) != code {
				return nil, errors.New("SALUS details device identity mismatch")
			}
			meta := map[string]json.RawMessage{}
			for _, k := range []string{"connected", "online", "current_temperature", "heating_setpoint", "cooling_setpoint", "running_state", "humidity", "temperature_display_mode", "firmware_version"} {
				if v, ok := details[k]; ok {
					meta[k] = v
				}
			}
			model := scalar(details["model"])
			if props, ok := salusProperties[model]; ok {
				v, _ := json.Marshal(props)
				meta["history_properties"] = v
			}
			devices = append(devices, Device{Source: "salus", ID: code, Name: scalar(details["name"]), Gateway: g.Code, Model: model, Metadata: meta})
		}
	}
	return devices, nil
}

type salusRecord struct {
	At    string          `json:"at"`
	Old   json.RawMessage `json:"old_value"`
	Value json.RawMessage `json:"value"`
}
type salusBundle struct {
	Total    *int          `json:"total_count"`
	Count    *int          `json:"count"`
	Complete *bool         `json:"complete"`
	Records  []salusRecord `json:"records"`
}
type salusPage struct {
	Device     string                 `json:"device_code"`
	From       string                 `json:"start_date"`
	To         string                 `json:"end_date"`
	Page       *int                   `json:"page"`
	Limit      *int                   `json:"limit"`
	Properties map[string]salusBundle `json:"properties"`
}

func (s *salusClient) history(ctx context.Context, d *dataset, dev Device, from, to time.Time, props []string) ([]Event, error) {
	const limit = 1000
	seen := map[string]int{}
	totals := map[string]int{}
	var events []Event
	// The specification does not define boundary inclusivity; overlap then clip locally.
	queryFrom := from.Add(-time.Second)
	for page := 0; ; page++ {
		q := url.Values{"start_date": {stamp(queryFrom.UTC())}, "end_date": {stamp(to.UTC())}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}, "order": {"asc"}}
		if len(props) > 0 {
			q.Set("property_names", strings.Join(props, ","))
		}
		raw, err := s.get(ctx, "/premium-lite/history/"+url.PathEscape(dev.ID)+"?"+q.Encode())
		p := salusPage{}
		if err == nil && json.Unmarshal(raw, &p) != nil {
			err = errors.New("invalid SALUS history response")
		}
		if err == nil && (p.Device != dev.ID || p.Page == nil || *p.Page != page || p.Limit == nil || *p.Limit != limit || p.Properties == nil) {
			err = errors.New("SALUS history page identity, pagination metadata, or properties missing")
		}
		if err == nil {
			start, e1 := time.Parse(time.RFC3339Nano, p.From)
			end, e2 := time.Parse(time.RFC3339Nano, p.To)
			if e1 != nil || e2 != nil || !start.Equal(queryFrom) || !end.Equal(to) {
				err = errors.New("SALUS response date bounds do not match request")
			}
			for _, property := range props {
				if _, ok := p.Properties[property]; !ok {
					err = errors.New("SALUS response omitted a requested history property")
				}
			}
		}
		allComplete := true
		count := 0
		if err == nil {
			if page > 0 && len(p.Properties) != len(totals) {
				err = errors.New("SALUS history property set changed between pages")
			}
			for property, b := range p.Properties {
				if b.Total == nil || b.Count == nil || b.Complete == nil || b.Records == nil || *b.Count != len(b.Records) || *b.Count > limit || *b.Total < 0 {
					err = errors.New("SALUS history property pagination metadata invalid")
					break
				}
				if page == 0 {
					totals[property] = *b.Total
				} else if n, ok := totals[property]; !ok || n != *b.Total {
					err = errors.New("SALUS history totals changed during pagination")
					break
				}
				seen[property] += len(b.Records)
				count += len(b.Records)
				if seen[property] > *b.Total || (*b.Complete && seen[property] != *b.Total) || (!*b.Complete && (*b.Count == 0 || seen[property] >= *b.Total)) {
					err = errors.New("SALUS history incomplete or inconsistent property counts")
					break
				}
				if !*b.Complete {
					allComplete = false
				}
				for _, r := range b.Records {
					at, e := time.Parse(time.RFC3339Nano, r.At)
					if e != nil || len(r.Value) == 0 {
						err = errors.New("SALUS event has invalid timestamp or missing value")
						break
					}
					if !at.Before(from) && at.Before(to) {
						events = append(events, Event{Device: dev, Property: property, At: at, RawAt: r.At, Value: r.Value, Old: r.Old})
					}
				}
				if err != nil {
					break
				}
			}
		}
		if e := d.record(Window{Source: "salus", Device: dev.ID, From: stamp(queryFrom.UTC()), To: stamp(to.UTC()), Page: &page, Rows: count}, raw, err); e != nil {
			return events, e
		}
		if err != nil {
			return events, err
		}
		if allComplete {
			return events, nil
		}
		if ctx.Err() != nil {
			return events, ctx.Err()
		}
	}
}
func exportSALUS(ctx context.Context, d *dataset, transport http.RoundTripper) error {
	s := salusClient{cfg: *d.Config.SALUS, wire: newWire(d.Config.SALUS.BaseURL, transport), scrub: d.Scrub}
	if err := s.login(ctx); err != nil {
		return d.record(Window{Source: "salus", Device: "authentication"}, nil, err)
	}
	devices, err := s.discover(ctx, d)
	if err != nil {
		return d.record(Window{Source: "salus", Device: "discovery"}, nil, err)
	}
	devices, err = choose(devices, s.cfg.IDs)
	if err != nil {
		return d.record(Window{Source: "salus", Device: "selection"}, nil, err)
	}
	d.Manifest.Devices = append(d.Manifest.Devices, devices...)
	if len(devices) == 0 {
		d.gap("SALUS discovery returned no selected devices")
	}
	for _, dev := range devices {
		props, known := salusProperties[dev.Model]
		if known && len(props) == 0 {
			d.gap("SALUS model has no requested history properties: " + dev.ID)
			continue
		}
		for _, property := range []string{"current_temperature", "heating_setpoint"} {
			if _, ok := s.cfg.Temperatures[dev.ID][property]; !ok {
				d.gap("SALUS " + property + " scale unverified; excluded from calculations: " + dev.ID)
			}
		}
		for cur := d.Range.Start.AddDate(0, 0, -s.cfg.LookbackDays); cur.Before(d.Range.End); {
			end := cur.AddDate(0, 0, 1)
			events, err := s.history(ctx, d, dev, cur, end, props)
			if err != nil {
				for i := range events {
					events[i].Quality = append(events[i].Quality, "partial_window")
				}
			}
			d.Events = append(d.Events, events...)
			d.Coverage = append(d.Coverage, Coverage{dev.ID, cur, end, err == nil})
			if err != nil {
				d.gap("SALUS history coverage gap: " + dev.ID + " " + cur.Format(time.DateOnly))
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cur = end
		}
	}
	return nil
}
func (d *dataset) temperature(device, property string, raw json.RawMessage) *float64 {
	if d.Config.SALUS == nil {
		return nil
	}
	rule, ok := d.Config.SALUS.Temperatures[device][property]
	if !ok {
		return nil
	}
	v := number(raw)
	if v == nil {
		return nil
	}
	if *v == 8000 || *v == 32768 || *v == -32768 {
		return nil
	}
	n := *v / rule.Divisor
	return &n
}
func runningState(raw json.RawMessage) (*bool, *bool) {
	var state map[string]json.RawMessage
	if json.Unmarshal(raw, &state) != nil {
		return nil, nil
	}
	var heat, cool bool
	if json.Unmarshal(state["heating"], &heat) != nil || json.Unmarshal(state["cooling"], &cool) != nil || string(state["heating"]) == "null" || string(state["cooling"]) == "null" {
		return nil, nil
	}
	return &heat, &cool
}
