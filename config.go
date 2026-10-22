package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	Timezone string         `json:"timezone"`
	Balance  *BalanceConfig `json:"balance"`
	SALUS    *SALUSConfig   `json:"salus"`
	Context  HeatingContext `json:"context"`
}
type BalanceConfig struct {
	BaseURL        string   `json:"base_url"`
	AllowHTTP      bool     `json:"allow_insecure_http"`
	Username       string   `json:"username"`
	Password       string   `json:"password"`
	IDs            []string `json:"meter_ids"`
	Granularity    string   `json:"granularity"`
	HourlyEvidence string   `json:"hourly_time_evidence"`
	GasFactor      *float64 `json:"gas_kwh_per_m3"`
	GasEvidence    string   `json:"gas_conversion_evidence"`
}
type TemperatureInterpretation struct {
	Divisor  float64 `json:"divisor"`
	Evidence string  `json:"evidence"`
}
type SALUSConfig struct {
	BaseURL      string   `json:"base_url"`
	Email        string   `json:"email"`
	Password     string   `json:"password"`
	IDs          []string `json:"device_ids"`
	LookbackDays int      `json:"initial_lookback_days"`
	// Device code -> property -> separately established interpretation.
	Temperatures map[string]map[string]TemperatureInterpretation `json:"temperature_interpretations"`
}
type HeatingContext struct {
	Area            *float64 `json:"apartment_area_m2,omitempty"`
	Boiler          string   `json:"boiler_type,omitempty"`
	FlowTemperature *float64 `json:"boiler_flow_temperature_c,omitempty"`
	Notes           string   `json:"notes,omitempty"`
}

func loadConfig(path, source string) (Config, *time.Location, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, nil, errors.New("cannot open configuration file")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, nil, errors.New("invalid configuration JSON or unknown field; see config.example.json")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, nil, errors.New("configuration must contain one JSON object")
	}
	if c.Timezone == "" {
		c.Timezone = "Europe/Chisinau"
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return c, nil, errors.New("unknown configuration timezone")
	}
	if source == "balance" {
		c.SALUS = nil
	}
	if source == "salus" {
		c.Balance = nil
	}
	if c.Balance == nil && c.SALUS == nil {
		return c, nil, errors.New("configure at least one selected source")
	}
	if b := c.Balance; b != nil {
		if b.BaseURL == "" {
			b.BaseURL = "http://djv-com.net:3000/v1/pv"
		}
		if err := validateBase(b.BaseURL, b.AllowHTTP); err != nil {
			return c, nil, fmt.Errorf("Balance: %w", err)
		}
		if b.Username == "" || b.Password == "" {
			return c, nil, errors.New("Balance credentials are required")
		}
		if b.Granularity == "" {
			b.Granularity = "daily"
		}
		if b.Granularity != "daily" && b.Granularity != "hourly" && b.Granularity != "monthly" {
			return c, nil, errors.New("Balance granularity must be daily, hourly, or monthly")
		}
		if b.GasFactor != nil && (!positive(*b.GasFactor) || strings.TrimSpace(b.GasEvidence) == "") {
			return c, nil, errors.New("gas conversion needs a positive factor and gas_conversion_evidence")
		}
	}
	if s := c.SALUS; s != nil {
		if s.BaseURL == "" {
			s.BaseURL = "https://openapi-eu.salusconnect.io"
		}
		if err := validateBase(s.BaseURL, false); err != nil {
			return c, nil, fmt.Errorf("SALUS: %w", err)
		}
		if s.Email == "" || s.Password == "" {
			return c, nil, errors.New("SALUS credentials are required")
		}
		if s.LookbackDays == 0 {
			s.LookbackDays = 7
		}
		if s.LookbackDays < 1 || s.LookbackDays > 90 {
			return c, nil, errors.New("initial_lookback_days must be between 1 and 90")
		}
		for _, props := range s.Temperatures {
			for property, v := range props {
				if property != "current_temperature" && property != "heating_setpoint" {
					return c, nil, errors.New("temperature interpretation property must be current_temperature or heating_setpoint")
				}
				if !positive(v.Divisor) || strings.TrimSpace(v.Evidence) == "" {
					return c, nil, errors.New("each temperature interpretation needs a positive divisor and evidence")
				}
			}
		}
	}
	if c.Context.Area != nil && !positive(*c.Context.Area) {
		return c, nil, errors.New("apartment area must be positive when supplied")
	}
	return c, loc, nil
}

func positive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func validateBase(s string, allowHTTP bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return errors.New("HTTPS is required; Balance's documented HTTP endpoint requires allow_insecure_http=true and exposes credentials in transit")
	}
	return nil
}

type DateRange struct{ Start, End time.Time } // Half-open; End is next local midnight after --to.
func parseRange(from, to string, loc *time.Location) (DateRange, error) {
	a, e1 := time.ParseInLocation(time.DateOnly, from, loc)
	b, e2 := time.ParseInLocation(time.DateOnly, to, loc)
	if e1 != nil || e2 != nil || a.After(b) {
		return DateRange{}, errors.New("--from and --to must be YYYY-MM-DD with from <= to (--to is inclusive)")
	}
	return DateRange{a, b.AddDate(0, 0, 1)}, nil
}
func (r DateRange) contains(t time.Time) bool    { return !t.Before(r.Start) && t.Before(r.End) }
func (r DateRange) overlaps(a, b time.Time) bool { return a.Before(r.End) && b.After(r.Start) }
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
func number(raw json.RawMessage) *float64 {
	s := scalar(raw)
	if s == "" {
		return nil
	}
	var n json.Number
	if json.Unmarshal([]byte(s), &n) != nil {
		return nil
	}
	v, err := n.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
func boolValue(raw json.RawMessage) *bool {
	s := scalar(raw)
	var v bool
	switch s {
	case "true", "t", "1":
		v = true
	case "false", "f", "0":
		v = false
	default:
		return nil
	}
	return &v
}
