package runner

import (
	"regexp"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
)

const isoLayout = "2006-01-02T15:04:05Z"

// ISO formats a UTC timestamp like the original runner.
func ISO(t time.Time) string { return t.UTC().Format(isoLayout) }

var hourRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:00:00Z$`)

// Timestamp parses an hour-aligned UTC timestamp argument.
func Timestamp(value string) (time.Time, error) {
	bad := config.Errorf("timestamp must be hour-aligned UTC")
	if !hourRE.MatchString(value) {
		return time.Time{}, bad
	}
	t, err := time.Parse(isoLayout, value)
	// Python's strftime('%Y') does not zero-pad years before 1000, so those
	// never round-trip and are rejected.
	if err != nil || t.Year() < 1000 || ISO(t) != value {
		return time.Time{}, bad
	}
	return t, nil
}

// Windows plans the bounded [start, end) windows of one invocation.
func Windows(args []string, env config.Env, now time.Time) ([][2]string, error) {
	now = now.UTC()
	if len(args) == 1 && args[0] == "hourly" {
		end := now.Truncate(time.Hour)
		hours, err := config.Positive(env, "RAWBBIT_DBT_HOURLY_LOOKBACK_HOURS", 3, 168)
		if err != nil {
			return nil, err
		}
		return [][2]string{{ISO(end.Add(-time.Duration(hours) * time.Hour)), ISO(end)}}, nil
	}
	if len(args) == 1 && args[0] == "daily" {
		end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		days, err := config.Positive(env, "RAWBBIT_DBT_DAILY_LOOKBACK_DAYS", 3, 7)
		if err != nil {
			return nil, err
		}
		return [][2]string{{ISO(end.AddDate(0, 0, -days)), ISO(end)}}, nil
	}
	if len(args) != 3 || args[0] != "backfill" {
		return nil, config.Errorf("usage: dbt-job {hourly|daily|backfill START_UTC END_UTC}")
	}
	start, err := Timestamp(args[1])
	if err != nil {
		return nil, err
	}
	end, err := Timestamp(args[2])
	if err != nil {
		return nil, err
	}
	if !start.Before(end) {
		return nil, config.Errorf("backfill end must be after start")
	}
	chunkHours, err := config.Positive(env, "RAWBBIT_DBT_BACKFILL_CHUNK_HOURS", 24, 168)
	if err != nil {
		return nil, err
	}
	if end.Sub(start) > 366*24*time.Hour {
		return nil, config.Errorf("backfill exceeds 366-day bound")
	}
	chunk := time.Duration(chunkHours) * time.Hour
	var result [][2]string
	for start.Before(end) {
		stop := start.Add(chunk)
		if stop.After(end) {
			stop = end
		}
		result = append(result, [2]string{ISO(start), ISO(stop)})
		start = stop
	}
	return result, nil
}
