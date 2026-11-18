package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// NegativeCase is one request the collector must reject.
type NegativeCase struct {
	Name       string
	WantStatus int
	Got        int
	Err        string
}

// OK reports whether the collector answered as expected.
func (n NegativeCase) OK() bool { return n.Err == "" && n.Got == n.WantStatus }

// RunNegative sends requests that must be rejected. Their events carry the
// run tag, so verify also proves that none of them was stored.
// maxEvents is the collector's MAX_EVENTS_PER_REQUEST (0 skips that case).
func RunNegative(ctx context.Context, c *Client, apiKey, runID, appID, environment string, maxEvents int) []NegativeCase {
	gen := NewGenerator(runID, appID, environment, 3, 0)
	one := func(mut func(*Event)) []byte {
		ev := gen.Next(time.Now())
		if mut != nil {
			mut(&ev)
		}
		b, _ := json.Marshal(Batch{Events: []Event{ev}})
		return b
	}
	type tc struct {
		name   string
		key    string
		body   []byte
		status int
	}
	cases := []tc{
		{"missing api key", "", one(nil), 401},
		{"invalid api key", "loadgen-invalid-" + runID, one(nil), 401},
		{"app_id not allowed for key", apiKey, one(func(e *Event) { e.AppID = "loadgen.forbidden." + runID }), 403},
		{"empty events", apiKey, []byte(`{"events":[]}`), 400},
		{"invalid json", apiKey, []byte(`{"events":[`), 422},
		{"missing event_name", apiKey, func() []byte {
			b := one(nil)
			var m map[string][]map[string]any
			_ = json.Unmarshal(b, &m)
			delete(m["events"][0], "event_name")
			out, _ := json.Marshal(m)
			return out
		}(), 422},
		{"bad event_timestamp", apiKey, one(func(e *Event) { e.EventTimestamp = "not-a-date" }), 422},
	}
	if maxEvents > 0 {
		evs := make([]Event, maxEvents+1)
		for i := range evs {
			evs[i] = gen.Next(time.Now())
		}
		b, _ := json.Marshal(Batch{Events: evs})
		cases = append(cases, tc{fmt.Sprintf("more than %d events", maxEvents), apiKey, b, 413})
	}
	out := make([]NegativeCase, 0, len(cases))
	for _, c0 := range cases {
		nc := NegativeCase{Name: c0.name, WantStatus: c0.status}
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
