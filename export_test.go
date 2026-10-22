package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func historyResponse(t *testing.T, r *http.Request, body string) *http.Response {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	p["start_date"] = r.URL.Query().Get("start_date")
	p["end_date"] = r.URL.Query().Get("end_date")
	b, _ := json.Marshal(p)
	return response(200, string(b))
}
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func testDataset(t *testing.T, from, to string) *dataset {
	t.Helper()
	loc, e := time.LoadLocation("Europe/Chisinau")
	if e != nil {
		t.Fatal(e)
	}
	r, e := parseRange(from, to, loc)
	if e != nil {
		t.Fatal(e)
	}
	c := Config{Timezone: loc.String(), Balance: &BalanceConfig{Granularity: "daily"}, SALUS: &SALUSConfig{Temperatures: map[string]map[string]TemperatureInterpretation{}}}
	d, e := newDataset(filepath.Join(t.TempDir(), "export"), c, r, loc, &scrubber{})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func csvRows(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	rows, e := csv.NewReader(f).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	var result []map[string]string
	for _, r := range rows[1:] {
		m := map[string]string{}
		for i, k := range rows[0] {
			m[k] = r[i]
		}
		result = append(result, m)
	}
	return result
}

func TestLocalDatesAndDST(t *testing.T) {
	for _, tc := range []struct {
		date  string
		hours int
	}{{"2026-03-29", 23}, {"2026-10-25", 25}, {"2026-10-19", 24}} {
		d := testDataset(t, tc.date, tc.date)
		if got := int(d.Range.End.Sub(d.Range.Start) / time.Hour); got != tc.hours {
			t.Fatalf("%s: %d hours", tc.date, got)
		}
		n := 0
		labels := map[string]bool{}
		for cur := d.Range.Start; cur.Before(d.Range.End); cur = d.binEnd(cur, "hourly") {
			labels[stamp(cur)] = true
			n++
		}
		if n != tc.hours || len(labels) != tc.hours {
			t.Fatal("DST hourly bins collapsed or invented")
		}
		dev := Device{Source: "salus", ID: "dst-thermostat"}
		d.Coverage = []Coverage{{dev.ID, d.Range.Start, d.Range.End, true}}
		addEvent(d, dev, "running_state", d.Range.Start, `{"heating":true,"cooling":false}`, `null`)
		d.prepare()
		var weightedDay weighted
		for _, s := range d.segments(dev) {
			weightedDay.add(s, d.Range.Start, d.Range.End)
		}
		if weightedDay.HeatSeconds != float64(tc.hours*3600) {
			t.Fatal("demand duration assumed 24-hour day")
		}
	}
	loc, _ := time.LoadLocation("Europe/Chisinau")
	if _, e := parseRange("2026-02-30", "2026-03-01", loc); e == nil {
		t.Fatal("invalid date accepted")
	}
}
func TestBalanceConsumptionMissingAndReset(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-03")
	var env struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if e := json.Unmarshal(fixture(t, "balance-daily.json"), &env); e != nil {
		t.Fatal(e)
	}
	dev := Device{Source: "balance", ID: "meter-a", Resource: "1", Unit: "m3"}
	d.balanceRows(dev, env.Data, d.Range.Start, d.Range.End)
	d.prepare()
	if len(d.Consumption) != 3 || d.Consumption[0].Value == nil || *d.Consumption[0].Value != 3.25 || d.Consumption[1].Value != nil || d.Consumption[2].Value == nil || *d.Consumption[2].Value != 0 {
		t.Fatal("missing converted to zero, or native ai values changed")
	}
	if strings.Contains(strings.Join(d.Consumption[2].Quality, ";"), "possible_reset") || scalar(d.Consumption[2].Raw["aa_day"]) != "2.0" {
		t.Fatal("comparison average decrease was called a counter reset, or raw average was lost")
	}
	v, seconds, _ := d.consumptionBin(dev, d.Range.Start, d.Range.End)
	if v != nil || seconds != 48*3600 {
		t.Fatal("incomplete range extrapolated")
	}
	if d.Consumption[0].KWh != nil {
		t.Fatal("gas converted without factor")
	}
	for _, raw := range []string{`"--"`, `null`, `"NaN"`, `"12oops"`, `"Infinity"`} {
		if number(json.RawMessage(raw)) != nil {
			t.Fatalf("accepted invalid number %s", raw)
		}
	}
}
func TestUnverifiedHourlyExcludedAndDSTCountChecked(t *testing.T) {
	d := testDataset(t, "2026-10-25", "2026-10-25")
	d.Config.Balance.Granularity = "hourly"
	dev := Device{Source: "balance", ID: "meter-a"}
	rows := make([]map[string]json.RawMessage, 24)
	for i := range rows {
		rows[i] = map[string]json.RawMessage{"ai_hour": json.RawMessage(`1`), "fai_hour": json.RawMessage(`"1"`)}
	}
	d.balanceRows(dev, rows, d.Range.Start, d.Range.End)
	if !d.Consumption[0].Start.IsZero() {
		t.Fatal("unverified index received timestamp")
	}
	d.Config.Balance.HourlyEvidence = "Synthetic test only: elapsed local hours, ordered rows"
	d.Consumption = nil
	d.balanceRows(dev, rows, d.Range.Start, d.Range.End)
	if !d.Consumption[0].Start.IsZero() {
		t.Fatal("24 rows mapped onto 25-hour day")
	}
	d.Consumption = nil
	rows = append(rows, rows[0])
	d.balanceRows(dev, rows, d.Range.Start, d.Range.End)
	if !d.Consumption[24].End.Equal(d.Range.End) {
		t.Fatal("confirmed DST mapping did not cover day")
	}
	v, seconds, _ := d.consumptionBin(dev, d.Range.Start, d.Range.End)
	if v == nil || *v != 25 || seconds != 25*3600 {
		t.Fatal("hourly aggregation wrong")
	}
}
func TestBalanceRefreshExactlyOnce(t *testing.T) {
	counts := map[string]int{}
	scrub := &scrubber{}
	b := balanceClient{cfg: BalanceConfig{Username: "fixture-user", Password: "fixture-password"}, scrub: scrub}
	b.wire = newWire("https://balance.test", roundTrip(func(r *http.Request) (*http.Response, error) {
		counts[r.URL.Path]++
		switch r.URL.Path {
		case "/loginwithdisableduser":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("login carries token")
			}
			return response(200, `{"data":{"token":"fixture-old-token","refresh_token":"fixture-refresh"}}`), nil
		case "/refreshtoken":
			if r.Header.Get("Authorization") != "Bearer fixture-refresh" {
				t.Fatal("wrong refresh auth")
			}
			return response(200, `{"data":{"token":"fixture-new-token"}}`), nil
		case "/read":
			if counts["/read"] == 1 {
				return response(200, `{"request_status":"error","error":{"error_type":5}}`), nil
			}
			if r.Header.Get("Authorization") != "Bearer fixture-new-token" {
				t.Fatal("replay uses stale access token")
			}
			return response(200, `{"data":[]}`), nil
		}
		t.Fatal("unexpected route")
		return nil, nil
	}))
	if e := b.login(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	if _, _, e := b.call(context.Background(), "GET", "/read", nil); e != nil {
		t.Fatal(e)
	}
	if counts["/read"] != 2 || counts["/refreshtoken"] != 1 {
		t.Fatal(counts)
	}
	counts["/read"] = 0
	b.wire.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		counts[r.URL.Path]++
		if r.URL.Path == "/refreshtoken" {
			return response(200, `{"data":{"token":"another-token"}}`), nil
		}
		return response(200, `{"request_status":"error","error":{"error_type":5}}`), nil
	})
	if _, _, e := b.call(context.Background(), "GET", "/read", nil); e == nil || counts["/read"] != 2 {
		t.Fatal("unbounded token replay or false success")
	}
}
func TestSALUSPerPropertyPagination(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "TEST-THERMOSTAT"}
	calls := 0
	s := salusClient{scrub: d.Scrub}
	s.wire = newWire("https://salus.test", roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Fatal("non-read history request")
		}
		q := r.URL.Query()
		if q.Get("order") != "asc" || q.Get("limit") != "1000" || q.Get("property_names") != "current_temperature,running_state" {
			t.Fatal(q)
		}
		if q.Get("start_date") != stamp(d.Range.Start.Add(-time.Second).UTC()) {
			t.Fatal("request did not use local boundary converted to UTC")
		}
		page := q.Get("page")
		calls++
		return historyResponse(t, r, string(fixture(t, "salus-page-"+page+".json"))), nil
	}))
	events, e := s.history(context.Background(), d, dev, d.Range.Start, d.Range.End, []string{"current_temperature", "running_state"})
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || len(events) != 4 {
		t.Fatalf("calls %d events %d", calls, len(events))
	}
	// An unfinished property with no progress must fail instead of looping.
	s.wire.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return historyResponse(t, r, `{"device_code":"TEST-THERMOSTAT","page":0,"limit":1000,"properties":{"running_state":{"total_count":2,"count":0,"complete":false,"records":[]}}}`), nil
	})
	if _, e = s.history(context.Background(), d, dev, d.Range.Start, d.Range.End, nil); e == nil {
		t.Fatal("non-progressing pagination accepted")
	}
}

func addEvent(d *dataset, dev Device, property string, at time.Time, value, old string) {
	d.Events = append(d.Events, Event{Device: dev, Property: property, At: at, RawAt: stamp(at.UTC()), Value: json.RawMessage(value), Old: json.RawMessage(old)})
}
func interpret(d *dataset, id string) {
	d.Config.SALUS.Temperatures[id] = map[string]TemperatureInterpretation{"current_temperature": {100, "Synthetic current-temperature fixture"}, "heating_setpoint": {10, "Separate synthetic setpoint fixture"}}
}
func TestWeightedEventsAndSeparateTemperatureScales(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "thermostat-a"}
	interpret(d, dev.ID)
	d.Coverage = []Coverage{{dev.ID, d.Range.Start.AddDate(0, 0, -1), d.Range.Start, true}, {dev.ID, d.Range.Start, d.Range.End, true}}
	mid := d.Range.Start.Add(12 * time.Hour)
	addEvent(d, dev, "current_temperature", mid, `"2200"`, `"2000"`)
	addEvent(d, dev, "heating_setpoint", mid, `"210"`, `"210"`)
	addEvent(d, dev, "running_state", mid, `{"heating":true,"cooling":false}`, `{"heating":false,"cooling":false}`)
	d.prepare()
	var w weighted
	for _, s := range d.segments(dev) {
		w.add(s, d.Range.Start, d.Range.End)
	}
	if w.TempSeconds != 86400 || mean(w.TempSum, w.TempSeconds) != "21" || mean(w.TargetSum, w.TargetSeconds) != "21" || w.HeatSeconds != 43200 || w.HeatKnown != 86400 || w.Over != 12 || w.Under != 12 {
		t.Fatalf("bad weighting: %+v", w)
	}
	delete(d.Config.SALUS.Temperatures[dev.ID], "current_temperature")
	w = weighted{}
	for _, s := range d.segments(dev) {
		w.add(s, d.Range.Start, d.Range.End)
	}
	if w.TempSeconds != 0 || w.JointSeconds != 0 || w.TargetSeconds != 86400 {
		t.Fatal("unverified property included or independent verified property excluded")
	}
	if h, _ := runningState(json.RawMessage(`1`)); h != nil {
		t.Fatal("scalar heating guessed")
	}
	if h, _ := runningState(json.RawMessage(`{"heating":null,"cooling":false}`)); h != nil {
		t.Fatal("null heating became false")
	}
}
func TestDisconnectionsFailuresAndUnknownInitial(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "thermostat-a", Metadata: map[string]json.RawMessage{"history_properties": json.RawMessage(`["connected","current_temperature"]`)}}
	interpret(d, dev.ID)
	d.Coverage = []Coverage{{dev.ID, d.Range.Start, d.Range.End, true}}
	start := d.Range.Start
	addEvent(d, dev, "connected", start, `true`, `null`)
	addEvent(d, dev, "current_temperature", start, `2000`, `null`)
	addEvent(d, dev, "connected", start.Add(6*time.Hour), `false`, `true`)
	addEvent(d, dev, "current_temperature", start.Add(8*time.Hour), `2100`, `null`)
	addEvent(d, dev, "connected", start.Add(12*time.Hour), `true`, `false`)
	addEvent(d, dev, "current_temperature", start.Add(18*time.Hour), `2200`, `null`)
	d.prepare()
	var w weighted
	for _, s := range d.segments(dev) {
		w.add(s, start, d.Range.End)
	}
	if w.TempSeconds != 12*3600 {
		t.Fatalf("disconnected/stale state counted: %+v", w)
	}
	d.Coverage = []Coverage{{dev.ID, start, start.Add(3 * time.Hour), true}, {dev.ID, start.Add(3 * time.Hour), start.Add(21 * time.Hour), false}, {dev.ID, start.Add(21 * time.Hour), d.Range.End, true}}
	w = weighted{}
	for _, s := range d.segments(dev) {
		w.add(s, start, d.Range.End)
	}
	if w.TempSeconds != 3*3600 {
		t.Fatal("state carried across failed query")
	}
	d.Events = nil
	w = weighted{}
	for _, s := range d.segments(dev) {
		w.add(s, start, d.Range.End)
	}
	if w.TempSeconds != 0 {
		t.Fatal("unknown initial state invented")
	}
}
func TestDuplicatesConflictsAndOutOfOrder(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "thermostat-a"}
	interpret(d, dev.ID)
	d.Coverage = []Coverage{{dev.ID, d.Range.Start, d.Range.End, true}}
	addEvent(d, dev, "current_temperature", d.Range.Start.Add(time.Hour), `2200`, `null`)
	addEvent(d, dev, "current_temperature", d.Range.Start, `2000`, `null`)
	addEvent(d, dev, "current_temperature", d.Range.Start, `2000`, `null`)
	addEvent(d, dev, "current_temperature", d.Range.Start, `2100`, `null`)
	d.prepare()
	if !d.Events[0].Conflict || !d.Events[1].Duplicate || !d.Events[2].Conflict {
		t.Fatal("duplicate/conflict not identified")
	}
	var w weighted
	for _, s := range d.segments(dev) {
		w.add(s, d.Range.Start, d.Range.End)
	}
	if w.TempSeconds != 23*3600 {
		t.Fatalf("conflicting timestamp carried: %+v", w)
	}
}
func TestRateLimitsAndBoundedRetries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := http.Header{"Retry-After": []string{"5"}, "Ratelimit": []string{"limit=1000,remaining=0,reset_in=12s"}}
	if waitDuration(h, now) != 12*time.Second || !exhausted(h) {
		t.Fatal("SALUS rate headers ignored")
	}
	h.Set("Retry-After", now.Add(time.Minute).Format(http.TimeFormat))
	if waitDuration(h, now) != time.Minute {
		t.Fatal("HTTP-date Retry-After ignored")
	}
	n := 0
	var sleeps []time.Duration
	w := newWire("https://api.test", roundTrip(func(*http.Request) (*http.Response, error) {
		n++
		if n < 3 {
			return response(503, `{}`), nil
		}
		return response(200, `[]`), nil
	}))
	w.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	if _, _, e := w.request(context.Background(), "GET", "/read", "", nil); e != nil || n != 3 || len(sleeps) != 2 {
		t.Fatal(n, sleeps, e)
	}
	n = 0
	w.next = time.Time{}
	w.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		n++
		r := response(429, `{}`)
		r.Header.Set("ratelimit", "remaining=0,reset_in=1124s")
		return r, nil
	})
	if _, _, e := w.request(context.Background(), "GET", "/read", "", nil); e == nil || n != 1 {
		t.Fatal("retried before server reset or waited unboundedly")
	}
}
func TestSecretScrubbingJSONAndCSV(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	secret := `secret-"quoted"-password`
	d.Scrub.add(secret, "fixture-access-token")
	raw, _ := json.Marshal(map[string]any{"value": secret, "nested": map[string]any{"wi_fi_password": "unknown-secret", "Authorization": "Bearer fixture-access-token"}})
	if e := d.record(Window{Source: "salus"}, raw, nil); e != nil {
		t.Fatal(e)
	}
	value, _ := json.Marshal(secret)
	if e := d.csv("scrub.csv", []string{"value"}, [][]string{{string(value)}}); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"raw/salus-000001.json", "scrub.csv"} {
		b, _ := os.ReadFile(filepath.Join(d.Dir, path))
		if bytes.Contains(b, []byte("quoted")) || bytes.Contains(b, []byte("unknown-secret")) || bytes.Contains(b, []byte("fixture-access-token")) {
			t.Fatal("secret escaped redaction:", string(b))
		}
	}
}

func TestCLICombinedExportAndPartialFailure(t *testing.T) {
	for _, failBalance := range []bool{false, true} {
		t.Run(strconv.FormatBool(failBalance), func(t *testing.T) {
			root := t.TempDir()
			config := Config{Timezone: "Europe/Chisinau", Balance: &BalanceConfig{BaseURL: "https://balance.test", Username: "fixture-user", Password: "fixture-password", Granularity: "daily"}, SALUS: &SALUSConfig{BaseURL: "https://salus.test", Email: "fixture@example.invalid", Password: "fixture-salus-password", LookbackDays: 1}}
			b, _ := json.Marshal(config)
			cfgPath := filepath.Join(root, "config.json")
			if e := os.WriteFile(cfgPath, b, 0600); e != nil {
				t.Fatal(e)
			}
			calls := 0
			rt := roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Host + r.URL.Path {
				case "balance.test/loginwithdisableduser":
					if failBalance {
						return response(401, `{"request_status":"error","error":{"error_type":15,"error_text":"fixture-password"}}`), nil
					}
					return response(200, `{"data":{"token":"fixture-access-token"}}`), nil
				case "balance.test/getmeterdescriptionwithlastdata":
					return response(200, `{"data":[{"tsd_id":"meter-a","resource_type":"1","counter_settings":{"custom_name":"Gas"}}]}`), nil
				case "balance.test/showslavemonthdata":
					return response(200, string(fixture(t, "balance-daily.json"))), nil
				case "salus.test/login":
					return response(200, `{"access_token":"fixture-salus-token","token_type":"Bearer"}`), nil
				case "salus.test/premium-lite/gateways":
					return response(200, `[{"device_code":"TEST-GATEWAY","name":"Home"}]`), nil
				case "salus.test/premium-lite/gateways/TEST-GATEWAY/devices":
					return response(200, `["TEST-THERMOSTAT"]`), nil
				case "salus.test/premium-lite/devices/TEST-THERMOSTAT":
					return response(200, `{"device_code":"TEST-THERMOSTAT","model":"SQ610","name":"Living room","connected":true,"wi_fi_password":"fixture-wifi-secret"}`), nil
				case "salus.test/premium-lite/history/TEST-THERMOSTAT":
					return historyResponse(t, r, `{"device_code":"TEST-THERMOSTAT","page":0,"limit":1000,"properties":{"current_temperature":{"total_count":0,"count":0,"complete":true,"records":[]},"heating_setpoint":{"total_count":0,"count":0,"complete":true,"records":[]},"running_state":{"total_count":0,"count":0,"complete":true,"records":[]}}}`), nil
				default:
					t.Fatalf("unexpected/control route %s %s", r.Method, r.URL)
					return nil, nil
				}
			})
			outDir := filepath.Join(root, "dataset")
			var output bytes.Buffer
			err := run(context.Background(), []string{"export", "--config", cfgPath, "--from", "2026-10-01", "--to", "2026-10-01", "--out", outDir}, &output, rt)
			if (err != nil) != failBalance {
				t.Fatal("unexpected export status", err)
			}
			var manifest Manifest
			blob, e := os.ReadFile(filepath.Join(outDir, "manifest.json"))
			if e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(blob, &manifest); e != nil {
				t.Fatal(e)
			}
			if (manifest.Status == "partial") != failBalance {
				t.Fatal(manifest.Status)
			}
			for _, name := range []string{"consumption.csv", "thermostat_events.csv", "analysis_aligned.csv", "analysis.md", "context.json", "devices.json"} {
				if _, e := os.Stat(filepath.Join(outDir, name)); e != nil {
					t.Fatal(e)
				}
			}
			if !failBalance {
				rows := csvRows(t, filepath.Join(outDir, "analysis_aligned.csv"))
				if len(rows) != 1 || rows[0]["consumption"] != "3.25" || rows[0]["room_temperature_mean_c"] != "" {
					t.Fatal(rows)
				}
			}
			_ = filepath.WalkDir(outDir, func(path string, entry os.DirEntry, e error) error {
				if e != nil {
					t.Fatal(e)
				}
				if entry.IsDir() {
					return nil
				}
				blob, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				for _, secret := range []string{"fixture-password", "fixture-salus-password", "fixture-access-token", "fixture-salus-token", "fixture-wifi-secret", "fixture@example.invalid"} {
					if bytes.Contains(blob, []byte(secret)) {
						t.Fatal("export leaked credential", path)
					}
				}
				return nil
			})
			before := calls
			if e := run(context.Background(), []string{"export", "--config", cfgPath, "--from", "2026-10-01", "--to", "2026-10-01", "--out", outDir}, io.Discard, rt); e == nil || calls != before {
				t.Fatal("existing export overwritten or network used first")
			}
		})
	}
}

func TestConfigAndSelectionValidation(t *testing.T) {
	if validateBase("http://djv-com.net:3000/v1/pv", false) == nil || validateBase("https://user:pass@example.com", false) == nil {
		t.Fatal("unsafe URL accepted")
	}
	if validateBase("http://djv-com.net:3000/v1/pv", true) != nil {
		t.Fatal("explicit HTTP configuration rejected")
	}
	if _, e := choose([]Device{{ID: "owned"}}, []string{"not-owned"}); e == nil {
		t.Fatal("unauthorized device selection accepted")
	}
	if number(json.RawMessage(`"0"`)) == nil || math.Abs(*number(json.RawMessage(`"23.5"`))-23.5) > 1e-9 {
		t.Fatal("scalar parsing")
	}
	var out bytes.Buffer
	if e := run(context.Background(), []string{"--help"}, &out, nil); e != nil || !strings.Contains(out.String(), "inclusive") {
		t.Fatal(fmt.Sprint(e))
	}
}

func TestContinuityMismatchExcludesInterveningState(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "thermostat-a"}
	interpret(d, dev.ID)
	d.Coverage = []Coverage{{dev.ID, d.Range.Start, d.Range.End, true}}
	addEvent(d, dev, "current_temperature", d.Range.Start, `2000`, `null`)
	addEvent(d, dev, "current_temperature", d.Range.Start.Add(6*time.Hour), `2200`, `2100`)
	d.prepare()
	var w weighted
	for _, s := range d.segments(dev) {
		w.add(s, d.Range.Start, d.Range.End)
	}
	if len(d.StateGaps) != 1 || w.TempSeconds != 18*3600 || mean(w.TempSum, w.TempSeconds) != "22" {
		t.Fatalf("unjustified carry: %+v gaps %+v", w, d.StateGaps)
	}
}
func TestMonthlyPartialWindowAndDocumentedGasConversion(t *testing.T) {
	d := testDataset(t, "2026-10-02", "2026-10-03")
	dev := Device{Source: "balance", ID: "gas-meter", Resource: "1", Unit: "m3"}
	d.Config.Balance.Granularity = "monthly"
	factor := 10.5
	d.Config.Balance.GasFactor = &factor
	d.Config.Balance.GasEvidence = "Synthetic documented factor"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, d.Loc)
	end := start.AddDate(1, 0, 0)
	d.balanceRows(dev, []map[string]json.RawMessage{{"pdate": json.RawMessage(`"2026-10-01"`), "ai_month": json.RawMessage(`"12"`), "fai_month": json.RawMessage(`"1"`)}}, start, end)
	if len(d.Consumption) != 1 || d.Consumption[0].KWh == nil || *d.Consumption[0].KWh != 126 {
		t.Fatal("documented gas conversion failed")
	}
	if value, _, _ := d.consumptionBin(dev, d.Range.Start, d.Range.End); value != nil {
		t.Fatal("monthly consumption prorated into partial dates")
	}
}
func TestSALUSRejectsWrongBoundsAndMissingProperty(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "TEST-THERMOSTAT"}
	s := salusClient{scrub: d.Scrub}
	for _, bad := range []string{`{"device_code":"TEST-THERMOSTAT","page":0,"limit":1000,"start_date":"2000-01-01T00:00:00Z","end_date":"2000-01-02T00:00:00Z","properties":{}}`, `{"device_code":"TEST-THERMOSTAT","page":0,"limit":1000,"properties":{}}`} {
		s.wire = newWire("https://salus.test", roundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(bad, "2000-") {
				return response(200, bad), nil
			}
			return historyResponse(t, r, bad), nil
		}))
		if _, e := s.history(context.Background(), d, dev, d.Range.Start, d.Range.End, []string{"running_state"}); e == nil {
			t.Fatal("missing property/wrong interval accepted")
		}
	}
}
func TestSingleSourceDiscoveryAndNoSecretMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	config := `{"balance":{"base_url":"http://unsafe.test","username":"ignored-user","password":"ignored-password"},"salus":{"email":"fixture@example.invalid","password":"fixture-password","base_url":"https://salus.test"}}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "salus.test" {
			t.Fatal("unselected source contacted")
		}
		switch r.URL.Path {
		case "/login":
			return response(200, `{"access_token":"fixture-secret-token","token_type":"Bearer"}`), nil
		case "/premium-lite/gateways":
			return response(200, `[{"device_code":"TEST-GATEWAY"}]`), nil
		case "/premium-lite/gateways/TEST-GATEWAY/devices":
			return response(200, `["TEST-THERMOSTAT"]`), nil
		case "/premium-lite/devices/TEST-THERMOSTAT":
			return response(200, `{"device_code":"TEST-THERMOSTAT","name":"Room","model":"SQ610","connected":true,"wi_fi_password":"vendor-secret"}`), nil
		default:
			t.Fatal("unexpected control/history in discovery")
			return nil, nil
		}
	})
	var out bytes.Buffer
	if err := run(context.Background(), []string{"discover", "--config", path, "--source", "salus"}, &out, rt); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-secret-token", "fixture-password", "vendor-secret", "fixture@example.invalid"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("discovery leaked secret")
		}
	}
	if !strings.Contains(out.String(), "TEST-THERMOSTAT") || !strings.Contains(out.String(), "history_properties") {
		t.Fatal("discovery missing useful metadata")
	}
}

func TestSALUS401RenewAndStopRejectedLogin(t *testing.T) {
	calls := map[string]int{}
	s := salusClient{token: "old-token", cfg: SALUSConfig{Email: "fixture@example.invalid", Password: "fixture-password"}, scrub: &scrubber{}}
	s.wire = newWire("https://salus.test", roundTrip(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Path]++
		if r.URL.Path == "/login" {
			return response(200, `{"access_token":"new-token","token_type":"Bearer"}`), nil
		}
		if r.Header.Get("Authorization") == "Bearer old-token" {
			return response(401, `{}`), nil
		}
		return response(200, `[]`), nil
	}))
	if _, err := s.get(context.Background(), "/read"); err != nil || calls["/login"] != 1 || calls["/read"] != 2 {
		t.Fatal("SALUS renew/replay", err, calls)
	}
	s.wire.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) { calls[r.URL.Path]++; return response(401, `{}`), nil })
	if _, err := s.get(context.Background(), "/read"); err == nil {
		t.Fatal("rejected renewal reported success")
	}
	before := calls["/read"]
	if _, err := s.get(context.Background(), "/read"); err == nil || calls["/read"] != before {
		t.Fatal("permanent auth rejection retried on later windows")
	}
}

func TestSALUSExclusiveStartBoundaryRetainsMidnightEvent(t *testing.T) {
	d := testDataset(t, "2026-10-01", "2026-10-01")
	dev := Device{Source: "salus", ID: "TEST-THERMOSTAT"}
	s := salusClient{scrub: d.Scrub}
	s.wire = newWire("https://salus.test", roundTrip(func(r *http.Request) (*http.Response, error) {
		from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("start_date"))
		if !d.Range.Start.After(from) {
			t.Fatal("no overlap for exclusive start")
		}
		body := fmt.Sprintf(`{"device_code":"TEST-THERMOSTAT","page":0,"limit":1000,"properties":{"running_state":{"total_count":1,"count":1,"complete":true,"records":[{"at":%q,"old_value":null,"value":{"heating":true,"cooling":false}}]}}}`, stamp(d.Range.Start.UTC()))
		return historyResponse(t, r, body), nil
	}))
	events, err := s.history(context.Background(), d, dev, d.Range.Start, d.Range.End, []string{"running_state"})
	if err != nil || len(events) != 1 || !events[0].At.Equal(d.Range.Start) {
		t.Fatal("lost exact-boundary event", err)
	}
}
