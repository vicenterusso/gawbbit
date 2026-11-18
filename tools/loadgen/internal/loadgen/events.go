// Package loadgen sends synthetic game events to a Rawbbit collector and
// verifies that every accepted event reached ClickHouse exactly once.
package loadgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"time"
)

// Event is one EventEnvelope as the collector accepts it.
type Event struct {
	EventID        string         `json:"event_id"`
	AppID          string         `json:"app_id"`
	Environment    string         `json:"environment"`
	EventName      string         `json:"event_name"`
	EventTimestamp string         `json:"event_timestamp"`
	User           map[string]any `json:"user"`
	Device         map[string]any `json:"device"`
	EventParams    map[string]any `json:"event_params"`
}

// Batch is the body of POST /v1/events:batch.
type Batch struct {
	Events []Event `json:"events"`
}

// RunParam is the event_params key that tags every event of a run.
const RunParam = "loadgen_run"

// eventMix is the weighted set of event names a synthetic player emits.
var eventMix = []struct {
	name   string
	weight int
}{
	{"level_started", 30},
	{"level_completed", 20},
	{"level_failed", 10},
	{"item_collected", 25},
	{"item_purchased", 5},
	{"settings_changed", 5},
	{"ad_watched", 5},
}

var platforms = []string{"webgl", "android", "ios", "windows", "macos", "linux"}

// NewRunID returns 12 random hex characters.
func NewRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Generator builds events for a fixed pool of synthetic players.
type Generator struct {
	RunID       string
	AppID       string
	Environment string
	Players     int
	rng         *mrand.Rand
	seq         int
	sessions    []string
	totalWeight int
}

// NewGenerator creates a generator. seed 0 picks a random seed.
func NewGenerator(runID, appID, environment string, players int, seed uint64) *Generator {
	if players < 1 {
		players = 1
	}
	if seed == 0 {
		seed = mrand.Uint64()
	}
	g := &Generator{RunID: runID, AppID: appID, Environment: environment, Players: players,
		rng: mrand.New(mrand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	for range players {
		g.sessions = append(g.sessions, NewUUID())
	}
	for _, m := range eventMix {
		g.totalWeight += m.weight
	}
	return g
}

func (g *Generator) pickName() string {
	n := g.rng.IntN(g.totalWeight)
	for _, m := range eventMix {
		if n < m.weight {
			return m.name
		}
		n -= m.weight
	}
	return eventMix[0].name
}

// Next returns a new event stamped with now.
func (g *Generator) Next(now time.Time) Event {
	g.seq++
	p := g.rng.IntN(g.Players)
	name := g.pickName()
	params := map[string]any{
		RunParam:      g.RunID,
		"loadgen_seq": g.seq,
	}
	switch name {
	case "level_started", "level_completed", "level_failed":
		params["level_id"] = 1 + g.rng.IntN(40)
		if name != "level_started" {
			params["duration_ms"] = 5_000 + g.rng.IntN(300_000)
			params["score"] = g.rng.IntN(100_000)
		}
	case "item_collected", "item_purchased":
		params["item_id"] = fmt.Sprintf("item_%02d", g.rng.IntN(30))
		if name == "item_purchased" {
			params["price_cents"] = []int{99, 199, 499, 999}[g.rng.IntN(4)]
			params["currency"] = "USD"
		}
	case "settings_changed":
		params["setting"] = []string{"volume", "quality", "language"}[g.rng.IntN(3)]
	case "ad_watched":
		params["placement"] = []string{"rewarded", "interstitial"}[g.rng.IntN(2)]
	}
	return Event{
		EventID:        NewUUID(),
		AppID:          g.AppID,
		Environment:    g.Environment,
		EventName:      name,
		EventTimestamp: now.UTC().Format("2006-01-02T15:04:05.000Z"),
		User: map[string]any{
			"user_pseudo_id": fmt.Sprintf("lg-%s-p%04d", g.RunID, p),
			"session_id":     g.sessions[p],
		},
		Device: map[string]any{
			"platform":    platforms[p%len(platforms)],
			"app_version": "1.0.0",
			"locale":      "en-US",
		},
		EventParams: params,
	}
}
