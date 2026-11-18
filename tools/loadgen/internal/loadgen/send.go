package loadgen

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// SendConfig controls a send run.
type SendConfig struct {
	CollectorURL string // base URL, e.g. https://collector.example.com
	HostHeader   string // optional Host override (reach Caddy by IP)
	APIKey       string
	AppID        string
	Environment  string
	RunID        string

	Events      int     // distinct events to deliver
	BatchSize   int     // events per request
	Rate        float64 // events per second, 0 = unlimited
	Concurrency int
	Players     int
	Duplicates  float64 // fraction of batches sent a second time
	Seed        uint64

	MaxRetries int
	Timeout    time.Duration
	Insecure   bool
}

// SendResult summarises a send run.
type SendResult struct {
	Manifest       Manifest
	Requests       int
	DuplicateSends int
	Retries        int
	Failed         int // distinct events never accepted
	Elapsed        time.Duration
	LatencyP50     time.Duration
	LatencyP95     time.Duration
	LatencyP99     time.Duration
	LatencyMax     time.Duration
}

// Client posts batches to the collector.
type Client struct {
	base, host, key string
	http            *http.Client
}

// NewClient creates a collector client.
func NewClient(base, host, key string, timeout time.Duration, insecure bool) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed test stacks
	}
	return &Client{base: strings.TrimRight(base, "/"), host: host, key: key,
		http: &http.Client{Timeout: timeout, Transport: tr}}
}

// Response is one collector reply.
type Response struct {
	Status int
	Body   []byte
}

// Post sends a raw body to /v1/events:batch. An empty key omits X-API-Key.
func (c *Client) Post(ctx context.Context, body []byte, key string) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/events:batch", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rawbbit-loadgen/1")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if c.host != "" {
		req.Host = c.host
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return Response{Status: resp.StatusCode, Body: b}, nil
}

// Health calls GET /healthz.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return err
	}
	if c.host != "" {
		req.Host = c.host
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func retryable(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusBadGateway ||
		status == http.StatusGatewayTimeout || status == http.StatusTooManyRequests || status >= 500
}

type job struct {
	body   []byte
	events []Event
	dup    bool
}

// Send delivers cfg.Events distinct events and returns the manifest of what
// the collector accepted. Each event is retried until accepted or
// MaxRetries is exhausted; failed batches are counted but not expected.
func Send(ctx context.Context, cfg SendConfig) (SendResult, error) {
	if cfg.BatchSize < 1 || cfg.Events < 1 || cfg.Concurrency < 1 {
		return SendResult{}, errors.New("events, batch size and concurrency must be positive")
	}
	client := NewClient(cfg.CollectorURL, cfg.HostHeader, cfg.APIKey, cfg.Timeout, cfg.Insecure)
	gen := NewGenerator(cfg.RunID, cfg.AppID, cfg.Environment, cfg.Players, cfg.Seed)

	var (
		mu        sync.Mutex
		res       SendResult
		latencies []time.Duration
		accepted  = map[string]Event{}
		minTS     time.Time
		maxTS     time.Time
	)
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				ok, retries, lat := postWithRetry(ctx, client, cfg, j.body)
				mu.Lock()
				res.Requests += 1 + retries
				res.Retries += retries
				latencies = append(latencies, lat...)
				if j.dup {
					res.DuplicateSends++
				}
				if ok {
					// A duplicate can land before (or instead of) its original.
					for _, ev := range j.events {
						accepted[ev.EventID] = ev
					}
				}
				mu.Unlock()
			}
		}()
	}

	start := time.Now()
	var interval time.Duration
	if cfg.Rate > 0 {
		interval = time.Duration(float64(cfg.BatchSize) / cfg.Rate * float64(time.Second))
	}
	next := start
	var dupQueue []job
	remaining := cfg.Events
	generated := 0
	stopped := false
	for remaining > 0 && !stopped {
		n := min(cfg.BatchSize, remaining)
		remaining -= n
		generated += n
		now := time.Now()
		evs := make([]Event, n)
		for i := range evs {
			evs[i] = gen.Next(now)
		}
		if minTS.IsZero() {
			minTS = now
		}
		maxTS = now
		body, _ := json.Marshal(Batch{Events: evs})
		j := job{body: body, events: evs}
		if cfg.Duplicates > 0 && gen.rng.Float64() < cfg.Duplicates {
			dupQueue = append(dupQueue, job{body: body, events: evs, dup: true})
		}
		if interval > 0 {
			if d := time.Until(next); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
				}
			}
			next = next.Add(interval)
		}
		select {
		case jobs <- j:
		case <-ctx.Done():
			stopped = true
		}
		// Re-send a duplicated batch a few batches later, well inside the
		// NATS duplicate window, so deduplication is exercised.
		if len(dupQueue) > 0 && (remaining == 0 || gen.rng.IntN(4) == 0) {
			d := dupQueue[0]
			dupQueue = dupQueue[1:]
			select {
			case jobs <- d:
			case <-ctx.Done():
				stopped = true
			}
		}
	}
	for _, d := range dupQueue {
		if stopped {
			break
		}
		select {
		case jobs <- d:
		case <-ctx.Done():
			stopped = true
		}
	}
	close(jobs)
	wg.Wait()
	res.Elapsed = time.Since(start)

	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	res.LatencyP50, res.LatencyP95, res.LatencyP99 = pct(latencies, 50), pct(latencies, 95), pct(latencies, 99)
	if len(latencies) > 0 {
		res.LatencyMax = latencies[len(latencies)-1]
	}
	m := Manifest{RunID: cfg.RunID, AppID: cfg.AppID, Environment: cfg.Environment, Expected: map[string]int{}}
	for _, ev := range accepted {
		m.Expected[ev.EventName]++
		m.Total++
	}
	if !minTS.IsZero() {
		m.WindowStart, m.WindowEnd = HourWindow(minTS, maxTS)
	}
	res.Failed = generated - m.Total
	res.Manifest = m
	if err := ctx.Err(); err != nil {
		return res, err
	}
	return res, nil
}

func postWithRetry(ctx context.Context, c *Client, cfg SendConfig, body []byte) (bool, int, []time.Duration) {
	var lat []time.Duration
	backoff := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		t0 := time.Now()
		resp, err := c.Post(ctx, body, cfg.APIKey)
		lat = append(lat, time.Since(t0))
		if err == nil && resp.Status == http.StatusOK {
			return true, attempt, lat
		}
		if err == nil && !retryable(resp.Status) {
			return false, attempt, lat
		}
		if attempt >= cfg.MaxRetries || ctx.Err() != nil {
			return false, attempt, lat
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false, attempt, lat
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p + 99) / 100
	return sorted[max(0, min(i, len(sorted))-1)]
}
