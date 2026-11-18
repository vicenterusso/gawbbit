// Package progress emits closed, bounded, best-effort progress lines on
// stderr. Only allowlisted labels and statuses are ever formatted; warehouse
// messages, SQL, paths, vars and credentials are never forwarded.
package progress

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
)

// Bounds of the emitter (unchanged from the original runner).
const (
	QueueLines   = 64
	MaxLine      = 1024
	CloseTimeout = 100 * time.Millisecond
	MaxElapsed   = 172800.0
)

// Labels are the approved (resource, node) pairs.
var Labels = map[[2]string]bool{
	{"model", "rawbbit_events_load"}: true,
	{"test", "not_null_event_id"}:    true,
	{"test", "not_null_app_id"}:      true,
	{"test", "not_null_event_time"}:  true,
}

// Statuses are the approved progress statuses per resource.
var Statuses = map[string]map[string]bool{
	"model": {"OK": true, "ERROR": true, "SKIP": true, "PARTIAL": true},
	"test":  {"PASS": true, "ERROR": true, "FAIL": true, "WARN": true, "SKIP": true},
}

// Emitter is one writer goroutine per invocation with a bounded queue. Only
// the goroutine may block on the sink; a stuck write is abandoned at exit.
type Emitter struct {
	queue  chan []byte
	done   chan struct{}
	failed atomic.Bool
	mu     sync.Mutex
	closed bool
}

// NewEmitter starts the writer goroutine for sink.
func NewEmitter(sink io.Writer) *Emitter {
	e := &Emitter{queue: make(chan []byte, QueueLines), done: make(chan struct{})}
	go func() {
		defer close(e.done)
		for data := range e.queue {
			if e.failed.Load() {
				continue
			}
			// Write once. Partial/broken writes are not retried.
			if _, err := sink.Write(data); err != nil {
				e.failed.Store(true)
			}
		}
	}()
	return e
}

// Emit queues a safe ASCII line, dropping it when full, closed or invalid.
func (e *Emitter) Emit(line string) {
	if len(line) >= MaxLine || e.failed.Load() {
		return
	}
	for i := 0; i < len(line); i++ {
		if line[i] >= 0x80 {
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	select {
	case e.queue <- []byte(line + "\n"):
	default:
	}
}

// Close stops accepting lines and waits a bounded time for queued writes.
func (e *Emitter) Close() {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.mu.Unlock()
	select {
	case <-e.done:
	case <-time.After(CloseTimeout):
	}
}

var databaseRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ErrInvalidContext rejects untrusted correlation fields.
var ErrInvalidContext = errors.New("invalid progress context")

// Prefix formats the parent-owned context. No app/request/user correlation.
func Prefix(now time.Time, job, attempt, kind, database string) (string, error) {
	if !clickhouse.IsUUID(job) || !clickhouse.IsUUID(attempt) {
		return "", ErrInvalidContext
	}
	if (kind != "default" && kind != "primary" && kind != "fallback") || !databaseRE.MatchString(database) {
		return "", ErrInvalidContext
	}
	return fmt.Sprintf("%s [dbt-progress job=%s attempt=%s kind=%s db=%s] ",
		now.UTC().Format("2006-01-02T15:04:05Z"), job, attempt, kind, database), nil
}

// NodeBody formats an observed node event, or "" when not approved.
// elapsed < 0 means unknown.
func NodeBody(resource, node, status string, elapsed float64) string {
	if !Labels[[2]string{resource, node}] {
		return ""
	}
	if status != "START" && !Statuses[resource][status] {
		return ""
	}
	body := status + " " + resource + " " + node
	if status != "START" && elapsed >= 0 && elapsed <= MaxElapsed && !math.IsNaN(elapsed) {
		body += fmt.Sprintf(" elapsed_seconds=%.3f", math.Round(elapsed*1000)/1000)
	}
	switch {
	case status == "FAIL" && resource == "test":
		body += " category=test_failed"
	case status == "ERROR":
		body += " category=node_error"
	}
	return body
}
