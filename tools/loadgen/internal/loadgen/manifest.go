package loadgen

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Manifest records what a send run delivered, for a later verify.
type Manifest struct {
	RunID       string `json:"run_id"`
	AppID       string `json:"app_id"`
	Environment string `json:"environment"`
	// WindowStart and WindowEnd are hour-aligned and cover every accepted
	// event timestamp: [WindowStart, WindowEnd).
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// Expected is the number of distinct accepted events per event_name.
	Expected map[string]int `json:"expected"`
	Total    int            `json:"total"`
}

// WriteManifest writes m as indented JSON.
func WriteManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadManifest reads a manifest written by WriteManifest.
func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if m.RunID == "" || m.AppID == "" {
		return m, fmt.Errorf("%s: run_id and app_id are required", path)
	}
	return m, nil
}

// HourWindow returns [floor(min), floor(max)+1h).
func HourWindow(min, max time.Time) (time.Time, time.Time) {
	return min.UTC().Truncate(time.Hour), max.UTC().Truncate(time.Hour).Add(time.Hour)
}
