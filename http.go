package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Authentication payloads are never recorded. All other persisted JSON passes through this scrubber.
type scrubber struct{ secrets []string }

func (s *scrubber) add(values ...string) {
	for _, v := range values {
		if v != "" {
			s.secrets = append(s.secrets, v)
		}
	}
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
}
func (s *scrubber) text(v string) string {
	for _, secret := range s.secrets {
		if len(secret) >= 4 {
			v = strings.ReplaceAll(v, secret, "[REDACTED]")
		} else if v == secret {
			v = "[REDACTED]"
		}
	}
	return v
}
func (s *scrubber) cell(v string) string {
	if json.Valid([]byte(v)) {
		if b, err := s.json(json.RawMessage(v)); err == nil {
			var compact bytes.Buffer
			if json.Compact(&compact, b) == nil {
				return compact.String()
			}
		}
	}
	return s.text(v)
}
func sensitiveKey(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
	return strings.Contains(k, "password") || strings.Contains(k, "token") || strings.Contains(k, "secret") || k == "authorization" || k == "cookie" || k == "setcookie" || k == "username" || k == "email" || k == "apikey"
}
func (s *scrubber) clean(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := make(map[string]any, len(x))
		for k, v := range x {
			if sensitiveKey(k) {
				o[s.text(k)] = "[REDACTED]"
			} else {
				o[s.text(k)] = s.clean(v)
			}
		}
		return o
	case []any:
		for i := range x {
			x[i] = s.clean(x[i])
		}
		return x
	case string:
		return s.text(x)
	case json.Number:
		for _, secret := range s.secrets {
			if x.String() == secret {
				return "[REDACTED]"
			}
		}
		return x
	default:
		return v
	}
}
func (s *scrubber) json(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var parsed any
	if err = d.Decode(&parsed); err != nil {
		return nil, err
	}
	return json.MarshalIndent(s.clean(parsed), "", "  ")
}

type apiError struct {
	Status int
	Code   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("API request failed (HTTP %d, code %s)", e.Status, e.Code)
}

type wire struct {
	client *http.Client
	base   string
	next   time.Time
	sleep  func(context.Context, time.Duration) error
}

func newWire(base string, transport http.RoundTripper) *wire {
	return &wire{base: strings.TrimRight(base, "/"), client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, sleep: pause}
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func waitDuration(h http.Header, now time.Time) time.Duration {
	var delay time.Duration
	if v := h.Get("Retry-After"); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil && seconds >= 0 {
			if seconds > 30 {
				delay = 31 * time.Second
			} else {
				delay = time.Duration(seconds) * time.Second
			}
		} else if t, err := http.ParseTime(v); err == nil {
			delay = t.Sub(now)
		}
	}
	if strings.Contains(h.Get("ratelimit"), "remaining=") && !exhausted(h) {
		return max(delay, 0)
	}
	for _, part := range strings.Split(h.Get("ratelimit"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == "reset_in" {
			if d, e := time.ParseDuration(v); e == nil && d > delay {
				delay = d
			}
		}
	}
	return max(delay, 0)
}
func responseDelay(h http.Header, body []byte, status int) time.Duration {
	delay := waitDuration(h, time.Now())
	if status == 429 {
		var envelope struct {
			Details struct {
				Reset string `json:"reset_in"`
			} `json:"details"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			if d, e := time.ParseDuration(envelope.Details.Reset); e == nil && d > delay {
				delay = d
			}
		}
	}
	return delay
}
func exhausted(h http.Header) bool {
	for _, p := range strings.Split(h.Get("ratelimit"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && k == "remaining" && v == "0" {
			return true
		}
	}
	return false
}

// Only this read-only application's login/history requests use this bounded retry helper.
func (w *wire) request(ctx context.Context, method, path, token string, body any) ([]byte, int, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, 0, errors.New("cannot encode request")
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if delay := time.Until(w.next); delay > 0 {
			if delay > 30*time.Second {
				return nil, 429, &apiError{429, "RATE_LIMIT_WAIT_EXCEEDS_30S"}
			}
			if err := w.sleep(ctx, delay); err != nil {
				return nil, 0, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, w.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, 0, errors.New("cannot construct request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := w.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			if attempt == 2 {
				return nil, 0, errors.New("network request failed after three attempts")
			}
			w.next = time.Now().Add(time.Duration(1<<attempt) * time.Second)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<20+1))
		closeErr := res.Body.Close()
		if readErr != nil || closeErr != nil {
			if attempt < 2 && ctx.Err() == nil {
				delay := max(waitDuration(res.Header, time.Now()), time.Duration(1<<attempt)*time.Second)
				if delay > 30*time.Second {
					return nil, res.StatusCode, &apiError{res.StatusCode, "RATE_LIMIT_WAIT_EXCEEDS_30S"}
				}
				w.next = time.Now().Add(delay)
				continue
			}
			return nil, res.StatusCode, errors.New("cannot read API response")
		}
		if len(data) > 32<<20 {
			return nil, res.StatusCode, errors.New("API response exceeds 32 MiB")
		}
		if exhausted(res.Header) || res.StatusCode == 429 {
			w.next = time.Now().Add(responseDelay(res.Header, data, res.StatusCode))
		}
		transient := res.StatusCode == 429 || res.StatusCode == 408 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504 || res.StatusCode == 500
		if transient && attempt < 2 {
			delay := max(responseDelay(res.Header, data, res.StatusCode), time.Duration(1<<attempt)*time.Second)
			if delay > 30*time.Second {
				return data, res.StatusCode, &apiError{res.StatusCode, "RATE_LIMIT_WAIT_EXCEEDS_30S"}
			}
			w.next = time.Now().Add(delay)
			continue
		}
		if !json.Valid(data) {
			return nil, res.StatusCode, &apiError{res.StatusCode, "NON_JSON_RESPONSE"}
		}
		return data, res.StatusCode, nil
	}
	return nil, 0, errors.New("request attempts exhausted")
}
