package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// NegativeCase is one request the collector must reject, or must accept
// while dropping its events.
type NegativeCase struct {
	Name       string
	WantStatus int
	// Dropped means the request is accepted (200) but its events must not be
	// stored. Its events carry the run tag, so verify proves they were not.
	Dropped bool
	Got     int
	Err     string
}

// OK reports whether the collector answered as expected.
func (n NegativeCase) OK() bool { return n.Err == "" && n.Got == n.WantStatus }

// RunNegative sends requests that must be rejected (wrong or missing API
// key, invalid JSON, a body over maxRequestBytes) and requests whose events
// the collector must drop (app_id not allowed for the key, missing
// event_name, bad event_timestamp). maxRequestBytes is the collector's
// MAX_REQUEST_BYTES (0 skips that case).
func RunNegative(ctx context.Context, c *Client, apiKey, runID, appID, environment string, maxRequestBytes int) []NegativeCase {
	gen := NewGenerator(runID, appID, environment, 3, 0)
	one := func(mut func(map[string]any)) []byte {
		ev := gen.Next(time.Now())
		b, _ := json.Marshal(ev)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if mut != nil {
			mut(m)
		}
		out, _ := json.Marshal(map[string]any{"events": []any{m}})
		return out
	}
	type tc struct {
		name    string
		key     string
		body    []byte
		status  int
		dropped bool
	}
	cases := []tc{
		{"missing api key", "", one(nil), 401, false},
		{"invalid api key", "loadgen-invalid-" + runID, one(nil), 401, false},
		{"invalid json", apiKey, []byte(`{"events":[`), 400, false},
		{"app_id not allowed for key", apiKey, one(func(m map[string]any) { m["app_id"] = "loadgen.forbidden." + runID }), 200, true},
		{"missing event_name", apiKey, one(func(m map[string]any) { delete(m, "event_name") }), 200, true},
		{"bad event_timestamp", apiKey, one(func(m map[string]any) { m["event_timestamp"] = "not-a-date" }), 200, true},
	}
	if maxRequestBytes > 0 {
		body := one(func(m map[string]any) {
			m["event_params"] = map[string]any{RunParam: runID, "padding": strings.Repeat("x", maxRequestBytes)}
		})
		cases = append(cases, tc{fmt.Sprintf("body over %d bytes", maxRequestBytes), apiKey, body, 413, false})
	}
	out := make([]NegativeCase, 0, len(cases))
	for _, c0 := range cases {
		nc := NegativeCase{Name: c0.name, WantStatus: c0.status, Dropped: c0.dropped}
		resp, err := c.Post(ctx, c0.body, c0.key)
		if err != nil {
			nc.Err = err.Error()
		} else {
			nc.Got = resp.Status
		}
		out = append(out, nc)
	}
	return out
}
