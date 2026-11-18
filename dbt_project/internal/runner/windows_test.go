package runner

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
)

// TestWindowsMatchPythonGolden replays windows() outputs recorded from the
// original runner over a generated corpus of arguments, settings and clocks.
func TestWindowsMatchPythonGolden(t *testing.T) {
	data, err := os.ReadFile("../../testdata/unit/windows.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Now     string            `json:"now"`
		Windows [][2]string       `json:"windows"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Windows(c.Args, config.Env(c.Env), now)
		if c.Error != "" {
			if cfg, ok := config.AsError(err); !ok || cfg.Msg != c.Error {
				t.Errorf("case %d %v %v: error %v, want %q", i, c.Args, c.Env, err, c.Error)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.Windows) {
			t.Errorf("case %d %v %v: got %v %v, want %v", i, c.Args, c.Env, got, err, c.Windows)
		}
	}
}

func TestHourlyDailyBackfillBounds(t *testing.T) {
	now := time.Date(2026, 1, 4, 5, 59, 0, 0, time.UTC)
	if got, _ := Windows([]string{"hourly"}, config.Env{}, now); got[0] != [2]string{"2026-01-04T02:00:00Z", "2026-01-04T05:00:00Z"} {
		t.Error(got)
	}
	if got, _ := Windows([]string{"daily"}, config.Env{}, now); got[0] != [2]string{"2026-01-01T00:00:00Z", "2026-01-04T00:00:00Z"} {
		t.Error(got)
	}
	for _, args := range [][]string{{"backfill", "2026-02-30T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"backfill", "2026-01-01T00:01:00Z", "2026-01-02T00:00:00Z"}} {
		if _, err := Windows(args, config.Env{}, now); err == nil {
			t.Error(args)
		}
	}
}
