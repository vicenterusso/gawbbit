//go:build integration

package publisher

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
)

// Run with: COLLECTOR_TEST_NATS_URL=nats://127.0.0.1:4222 go test -tags integration ./internal/publisher/
func TestJetStreamIntegration(t *testing.T) {
	url := os.Getenv("COLLECTOR_TEST_NATS_URL")
	if url == "" {
		t.Skip("COLLECTOR_TEST_NATS_URL not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable: %v", err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	const stream = "COLLECTOR_IT"
	_ = js.DeleteStream(ctx, stream)
	defer js.DeleteStream(ctx, stream)

	var logs bytes.Buffer
	p := New(Config{URL: url, Stream: stream, SubjectPrefix: "collectorit", StreamMaxAgeSeconds: 3600,
		DuplicateWindowSeconds: 120, MaxInflightPublishes: 4}, logx.New(&logs, logx.LevelDebug))
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.CachedInfo().Config
	if cfg.Storage != jetstream.FileStorage || cfg.MaxAge != time.Hour || cfg.Duplicates != 2*time.Minute ||
		len(cfg.Subjects) != 1 || cfg.Subjects[0] != "collectorit.>" {
		t.Fatalf("stream config %+v", cfg)
	}

	batch := []Message{
		{Subject: "collectorit.app", Payload: []byte(`{"n":1}`), MsgID: "app:1"},
		{Subject: "collectorit.app", Payload: []byte(`{"n":2}`), MsgID: "app:2"},
		{Subject: "collectorit.app", Payload: []byte(`{"n":1}`), MsgID: "app:1"}, // duplicate
	}
	if err := p.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// A retried batch is deduplicated by Nats-Msg-Id.
	if err := p.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	info, _ := s.Info(ctx)
	if info.State.Msgs != 2 {
		t.Fatalf("stored %d messages, want 2", info.State.Msgs)
	}
	msg, err := s.GetMsg(ctx, 1)
	if err != nil || msg.Header.Get("Nats-Msg-Id") == "" {
		t.Fatalf("message %+v %v", msg, err)
	}

	// Reconnecting to the existing stream is accepted.
	p2 := New(Config{URL: url, Stream: stream, SubjectPrefix: "collectorit", StreamMaxAgeSeconds: 3600,
		DuplicateWindowSeconds: 120, MaxInflightPublishes: 4}, logx.New(&logs, logx.LevelDebug))
	if err := p2.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	p2.Close()

	// Publishing without a stream fails like nats-py (no responders, no retry).
	_ = js.DeleteStream(ctx, stream)
	err = p.PublishBatch(ctx, batch[:1])
	be, ok := err.(*BatchPublishError)
	if !ok || ErrorClass(be.FirstError) != "NoStreamResponseError" {
		t.Fatalf("got %v", err)
	}
}
