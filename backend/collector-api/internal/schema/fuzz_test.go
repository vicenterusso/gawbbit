package schema

import "testing"

// FuzzParseBatch checks that validation never panics and that accepted
// events can always be rendered.
func FuzzParseBatch(f *testing.F) {
	f.Add([]byte(`{"events":[{"event_id":"e","app_id":"a","event_name":"n","event_timestamp":"2026-03-20T18:42:15Z"}]}`))
	f.Add([]byte(`{"events":[{"event_timestamp":1700000000.5,"consent":{"ads_storage":"yes"},"event_params":{"x":NaN}}]}`))
	f.Add([]byte(`{"events":[{}, 1, null]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		events, _ := ParseBatch(body)
		for i := range events {
			_ = events[i].Dump()
		}
	})
}

func FuzzDatetimeString(f *testing.F) {
	for _, s := range []string{"2026-03-20T18:42:15.123+05:30", "1700000000.5", "-1", "2026-03-20", "9999-12-31T23:59:59−23:59"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if dt, err := parseDateTimeString([]byte(s)); err == nil {
			_ = dt.String()
		}
		_, _ = parseDateString([]byte(s))
	})
}
