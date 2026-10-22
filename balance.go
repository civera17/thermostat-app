package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type balanceClient struct {
	cfg            BalanceConfig
	wire           *wire
	scrub          *scrubber
	token, refresh string
	authFailed     bool
}
type balanceEnvelope struct {
	Status string          `json:"request_status"`
	Data   json.RawMessage `json:"data"`
	Error  *struct {
		Code json.RawMessage `json:"error_type"`
	} `json:"error"`
}

func (b *balanceClient) login(ctx context.Context, refresh bool) error {
	path, token := "/loginwithdisableduser", ""
	var body any = map[string]string{"username": b.cfg.Username, "password": b.cfg.Password}
	if refresh && b.refresh != "" {
		path = "/refreshtoken"
		token = b.refresh
		body = struct{}{}
	}
	raw, status, err := b.wire.request(ctx, "POST", path, token, body)
	if err != nil {
		return err
	}
	data, err := decodeBalance(raw, status)
	if err != nil {
		b.authFailed = true
		return err
	}
	var session struct {
		Token   string `json:"token"`
		Refresh string `json:"refresh_token"`
	}
	if json.Unmarshal(data, &session) != nil || session.Token == "" {
		b.authFailed = true
		return errors.New("Balance login returned no token")
	}
	b.token = session.Token
	if session.Refresh != "" {
		b.refresh = session.Refresh
	}
	b.scrub.add(b.token, b.refresh)
	return nil
}
func decodeBalance(raw []byte, status int) (json.RawMessage, error) {
	var env balanceEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return nil, errors.New("invalid Balance envelope")
	}
	if env.Status == "error" || status < 200 || status >= 300 {
		code := "UNKNOWN"
		if env.Error != nil {
			if _, err := strconv.Atoi(scalar(env.Error.Code)); err == nil {
				code = scalar(env.Error.Code)
			}
		}
		return nil, &apiError{status, "BALANCE_" + code}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, errors.New("Balance response missing data")
	}
	return env.Data, nil
}
func (b *balanceClient) call(ctx context.Context, method, path string, body any) (json.RawMessage, []byte, error) {
	if b.authFailed {
		return nil, nil, errors.New("Balance session unavailable after authentication rejection")
	}
	refreshed := false
	for attempt := 0; attempt < 4; attempt++ {
		raw, status, err := b.wire.request(ctx, method, path, b.token, body)
		if err != nil {
			return nil, raw, err
		}
		data, err := decodeBalance(raw, status)
		if err == nil {
			return data, raw, nil
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "BALANCE_5" && !refreshed {
			refreshed = true
			if e := b.login(ctx, true); e != nil {
				return nil, raw, e
			}
			continue
		}
		if errors.As(err, &ae) && ae.Code == "BALANCE_11" && attempt < 2 {
			b.wire.next = maxTime(b.wire.next, time.Now().Add(time.Duration(1<<attempt)*time.Second))
			continue
		}
		return nil, raw, err
	}
	return nil, nil, errors.New("Balance request retries exhausted")
}
func (b *balanceClient) discover(ctx context.Context) ([]Device, []byte, error) {
	data, raw, err := b.call(ctx, "GET", "/getmeterdescriptionwithlastdata", nil)
	if err != nil {
		return nil, raw, err
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(data, &rows) != nil || string(data) == "null" {
		return nil, raw, errors.New("Balance discovery data is not an array")
	}
	var devices []Device
	seen := map[string]bool{}
	for _, row := range rows {
		id := scalar(row["tsd_id"])
		if id == "" || seen[id] {
			return nil, raw, errors.New("Balance discovery has missing or duplicate meter IDs")
		}
		seen[id] = true
		var settings map[string]json.RawMessage
		_ = json.Unmarshal(row["counter_settings"], &settings)
		unit := map[string]string{"1": "m3", "8": "m3", "4": "kWh", "2": "Gcal", "16": "Gcal"}[scalar(row["resource_type"])]
		meta := map[string]json.RawMessage{}
		for _, k := range []string{"slave_uid", "slave_usn", "resource_name", "resource_type", "last_day", "counter_indications"} {
			if v, ok := row[k]; ok {
				meta[k] = v
			}
		}
		devices = append(devices, Device{Source: "balance", ID: id, WireID: row["tsd_id"], Name: scalar(settings["custom_name"]), Resource: scalar(row["resource_type"]), Unit: unit, Metadata: meta})
	}
	return devices, raw, nil
}
func balanceSuffix(g string) string {
	switch g {
	case "hourly":
		return "hour"
	case "monthly":
		return "month"
	default:
		return "day"
	}
}
func balanceDate(raw json.RawMessage) (string, bool) {
	s := scalar(raw)
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.DateOnly), true
		}
	}
	return "", false
}
func (d *dataset) balanceRows(dev Device, rows []map[string]json.RawMessage, from, to time.Time) {
	cfg := d.Config.Balance
	g := cfg.Granularity
	suffix := balanceSuffix(g)
	expected := int(to.Sub(from) / time.Hour)
	for i, row := range rows {
		c := Consumption{Device: dev, Granularity: g, From: from.Format(time.DateOnly), To: to.AddDate(0, 0, -1).Format(time.DateOnly), Raw: row}
		if g == "hourly" {
			idx := i
			c.Index = &idx
			c.Date = from.Format(time.DateOnly)
			c.TimeBasis = "unverified_hour_index"
			if cfg.HourlyEvidence != "" && len(rows) == expected {
				c.Start = from.Add(time.Duration(i) * time.Hour)
				c.End = c.Start.Add(time.Hour)
				c.TimeBasis = "user_evidence_elapsed_local_hours"
			} else {
				c.Quality = append(c.Quality, "hourly_timestamp_unverified")
				d.gap("Balance hourly timestamps excluded from alignment: " + dev.ID + " " + c.Date)
			}
		} else {
			c.PDate = scalar(row["pdate"])
			date, ok := balanceDate(row["pdate"])
			if ok {
				c.Date = date
				c.Start, _ = time.ParseInLocation(time.DateOnly, date, d.Loc)
				c.End = c.Start.AddDate(0, 0, 1)
				if g == "monthly" {
					c.Start = time.Date(c.Start.Year(), c.Start.Month(), 1, 0, 0, 0, 0, d.Loc)
					c.End = c.Start.AddDate(0, 1, 0)
				}
				c.TimeBasis = "calendar_label_configured_timezone_assumption"
			} else {
				c.Quality = append(c.Quality, "pdate_unparseable")
				d.gap("Balance unparseable pdate: " + dev.ID)
			}
		}
		if !c.Start.IsZero() && !d.Range.overlaps(c.Start, c.End) {
			continue
		}
		c.Value = number(row["ai_"+suffix])
		flag := scalar(row["fai_"+suffix])
		flagNumber, flagErr := strconv.Atoi(flag)
		noData := boolValue(row["no_data"])
		if c.Value == nil || flagErr != nil || flagNumber <= 0 || flagNumber == 4 || (noData != nil && *noData) {
			c.Value = nil
			c.Quality = append(c.Quality, "missing_or_invalid_value_or_flag")
		}
		if c.Value != nil && *c.Value < 0 {
			c.Value = nil
			c.Quality = append(c.Quality, "negative_consumption_possible_reset")
		}
		if c.Value != nil && dev.Resource == "1" && dev.Unit == "m3" && cfg.GasFactor != nil {
			v := *c.Value * *cfg.GasFactor
			c.KWh = &v
		}
		if !c.Start.IsZero() && (c.Start.Before(d.Range.Start) || c.End.After(d.Range.End)) {
			c.Quality = append(c.Quality, "partial_requested_period_not_prorated")
		}
		d.Consumption = append(d.Consumption, c)
	}
	if len(rows) == 0 {
		d.gap("Balance empty history window: " + dev.ID + " " + from.Format(time.DateOnly))
	}
}
func exportBalance(ctx context.Context, d *dataset, transport http.RoundTripper) error {
	b := balanceClient{cfg: *d.Config.Balance, wire: newWire(d.Config.Balance.BaseURL, transport), scrub: d.Scrub}
	if err := b.login(ctx, false); err != nil {
		return d.record(Window{Source: "balance", Device: "authentication"}, nil, err)
	}
	devices, raw, err := b.discover(ctx)
	if e := d.record(Window{Source: "balance", Device: "discovery", Rows: len(devices)}, raw, err); e != nil {
		return e
	}
	if err != nil {
		return nil
	}
	devices, err = choose(devices, b.cfg.IDs)
	if err != nil {
		return d.record(Window{Source: "balance", Device: "selection"}, nil, err)
	}
	d.Manifest.Devices = append(d.Manifest.Devices, devices...)
	if len(devices) == 0 {
		d.gap("Balance discovery returned no selected meters")
	}
	for _, dev := range devices {
		start := d.Range.Start
		g := b.cfg.Granularity
		if g == "daily" {
			start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, d.Loc)
		}
		if g == "monthly" {
			start = time.Date(start.Year(), 1, 1, 0, 0, 0, 0, d.Loc)
		}
		for cur := start; cur.Before(d.Range.End); {
			end := cur.AddDate(0, 0, 1)
			path := "/showslavedaydata"
			if g == "daily" {
				end = cur.AddDate(0, 1, 0)
				path = "/showslavemonthdata"
			}
			if g == "monthly" {
				end = cur.AddDate(1, 0, 0)
				path = "/showslaveyeardata"
			}
			body := map[string]any{"tsd_id": dev.WireID, "flag_balance": "0", "flag_hour": "0", "with_price": false, "date00": cur.Format(time.DateOnly), "date01": end.AddDate(0, 0, -1).Format(time.DateOnly)}
			data, raw, err := b.call(ctx, "POST", path, body)
			var rows []map[string]json.RawMessage
			if err == nil && (json.Unmarshal(data, &rows) != nil || strings.TrimSpace(string(data)) == "null") {
				err = errors.New("Balance history data is not an array")
			}
			if e := d.record(Window{Source: "balance", Device: dev.ID, From: cur.Format(time.DateOnly), To: end.AddDate(0, 0, -1).Format(time.DateOnly), Rows: len(rows)}, raw, err); e != nil {
				return e
			}
			if err == nil {
				d.balanceRows(dev, rows, cur, end)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cur = end
		}
	}
	return nil
}
