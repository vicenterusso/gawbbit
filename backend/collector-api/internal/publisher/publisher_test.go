package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
)

type published struct{ subject, payload, msgID string }

type fakeJetStream struct {
	delay    time.Duration
	failing  map[string]bool
	inflight atomic.Int32
	max      atomic.Int32

	mu        sync.Mutex
	published []published

	// stream management
	info, addResult    *jetstream.StreamConfig
	addErr, infoErr    error
	followupInfoErr    error
	createdAfterAddErr *jetstream.StreamConfig
	calls              []string
	addConfigs         []jetstream.StreamConfig
}

func (f *fakeJetStream) Publish(ctx context.Context, subject string, payload []byte, msgID string) error {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	f.published = append(f.published, published{subject, string(payload), msgID})
	f.mu.Unlock()
	if f.failing[msgID] {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeJetStream) StreamInfo(context.Context, string) (*jetstream.StreamConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stream_info")
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	if len(f.calls) > 1 && f.followupInfoErr != nil {
		return nil, f.followupInfoErr
	}
	if f.info == nil {
		return nil, jetstream.ErrStreamNotFound
	}
	return f.info, nil
}

func (f *fakeJetStream) AddStream(_ context.Context, cfg jetstream.StreamConfig) (*jetstream.StreamConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "add_stream")
	f.addConfigs = append(f.addConfigs, cfg)
	if f.addErr != nil {
		f.info = f.createdAfterAddErr
		return nil, f.addErr
	}
	f.info = f.addResult
	return f.addResult, nil
}

func newTestPublisher(js JetStream, maxInflight int64) (*Publisher, *bytes.Buffer) {
	var logs bytes.Buffer
	p := NewWithJetStream(Config{
		URL: "nats://nats:4222", Stream: "EVENTS", SubjectPrefix: "events",
		StreamMaxAgeSeconds: 86400, DuplicateWindowSeconds: 600, MaxInflightPublishes: maxInflight,
	}, logx.New(&logs, logx.LevelDebug), js)
	return p, &logs
}

func messages(n int) []Message {
	out := make([]Message, n)
	for i := range out {
		out[i] = Message{Subject: "events.app", Payload: []byte(fmt.Sprintf(`{"event_id":"e%d"}`, i)), MsgID: fmt.Sprintf("app:e%d", i)}
	}
	return out
}

func TestBatchIsPublishedConcurrently(t *testing.T) {
	js := &fakeJetStream{delay: 50 * time.Millisecond}
	p, _ := newTestPublisher(js, 512)
	started := time.Now()
	if err := p.PublishBatch(context.Background(), messages(50)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 50*50*time.Millisecond/5 {
		t.Errorf("took %v", elapsed)
	}
	if len(js.published) != 50 || js.max.Load() != 50 {
		t.Errorf("published=%d max inflight=%d", len(js.published), js.max.Load())
	}
}

func TestInflightPublishesAreCapped(t *testing.T) {
	js := &fakeJetStream{delay: 10 * time.Millisecond}
	p, _ := newTestPublisher(js, 8)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.PublishBatch(context.Background(), messages(30)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if js.max.Load() != 8 || len(js.published) != 60 {
		t.Errorf("max inflight=%d published=%d", js.max.Load(), len(js.published))
	}
}

func TestFailureReportsCountsAfterAttemptingEveryEvent(t *testing.T) {
	js := &fakeJetStream{failing: map[string]bool{"app:e3": true}}
	p, _ := newTestPublisher(js, 512)
	err := p.PublishBatch(context.Background(), messages(10))
	var batchErr *BatchPublishError
	if !errors.As(err, &batchErr) {
		t.Fatalf("got %v", err)
	}
	if len(js.published) != 10 || batchErr.Published != 9 || batchErr.Failed != 1 ||
		ErrorClass(batchErr.FirstError) != "TimeoutError" {
		t.Errorf("published=%d %+v", len(js.published), batchErr)
	}
}

func TestPublishStripsMessageID(t *testing.T) {
	js := &fakeJetStream{}
	p, _ := newTestPublisher(js, 1)
	if err := p.Publish(context.Background(), Message{Subject: "s", MsgID: " app:e1 \t"}); err != nil {
		t.Fatal(err)
	}
	if js.published[0].msgID != "app:e1" {
		t.Errorf("%q", js.published[0].msgID)
	}
}

func streamInfo(subjects []string, storage jetstream.StorageType) *jetstream.StreamConfig {
	if subjects == nil {
		subjects = []string{"events.>"}
	}
	return &jetstream.StreamConfig{Subjects: subjects, Storage: storage}
}

func TestExistingCompatibleStreamIsCheckedWithoutAdd(t *testing.T) {
	js := &fakeJetStream{info: streamInfo(nil, jetstream.FileStorage)}
	p, logs := newTestPublisher(js, 1)
	if err := p.EnsureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(js.calls, ",") != "stream_info" ||
		!strings.Contains(logs.String(), "stream_exists_and_compatible stream=EVENTS subjects=['events.>']") {
		t.Errorf("calls=%v logs=%s", js.calls, logs)
	}
}

func TestNotFoundThenSuccessfulAddLogsCreated(t *testing.T) {
	js := &fakeJetStream{addResult: streamInfo(nil, jetstream.FileStorage)}
	p, logs := newTestPublisher(js, 1)
	if err := p.EnsureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(js.calls, ",") != "stream_info,add_stream" || !strings.Contains(logs.String(), "stream_created stream=EVENTS") {
		t.Errorf("calls=%v logs=%s", js.calls, logs)
	}
	cfg := js.addConfigs[0]
	if cfg.Name != "EVENTS" || strings.Join(cfg.Subjects, ",") != "events.>" || cfg.Storage != jetstream.FileStorage ||
		cfg.MaxAge != 86400*time.Second || cfg.Duplicates != 600*time.Second {
		t.Errorf("config %+v", cfg)
	}
}

func TestSuccessfulAddResultIsCheckedForCompatibility(t *testing.T) {
	for _, tc := range []struct {
		result *jetstream.StreamConfig
		msg    string
	}{
		{streamInfo([]string{"other.>"}, jetstream.FileStorage), "incompatible subjects"},
		{streamInfo(nil, jetstream.MemoryStorage), "uses storage 'memory', expected FILE"},
	} {
		p, _ := newTestPublisher(&fakeJetStream{addResult: tc.result}, 1)
		if err := p.EnsureStream(context.Background()); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("got %v, want %q", err, tc.msg)
		}
	}
}

func TestExistingIncompatibleSubjectsAndStorageKeepErrors(t *testing.T) {
	for _, tc := range []struct {
		info *jetstream.StreamConfig
		msg  string
	}{
		{streamInfo([]string{"other.>", "another.x"}, jetstream.FileStorage),
			"JetStream stream 'EVENTS' exists with incompatible subjects ['another.x', 'other.>']; expected to include 'events.>'"},
		{streamInfo(nil, jetstream.MemoryStorage), "JetStream stream 'EVENTS' uses storage 'memory', expected FILE"},
	} {
		js := &fakeJetStream{info: tc.info}
		p, _ := newTestPublisher(js, 1)
		if err := p.EnsureStream(context.Background()); err == nil || err.Error() != tc.msg {
			t.Errorf("got %v, want %q", err, tc.msg)
		}
		if strings.Join(js.calls, ",") != "stream_info" {
			t.Errorf("calls %v", js.calls)
		}
	}
}

func TestNonNotFoundStreamInfoErrorIsNotTreatedAsAbsence(t *testing.T) {
	denied := errors.New("stream info denied")
	js := &fakeJetStream{infoErr: denied}
	p, _ := newTestPublisher(js, 1)
	if err := p.EnsureStream(context.Background()); err != denied || strings.Join(js.calls, ",") != "stream_info" {
		t.Errorf("err=%v calls=%v", err, js.calls)
	}
}

func TestAddErrorRaceConfirmsExistingCompatibleStream(t *testing.T) {
	js := &fakeJetStream{addErr: errors.New("stream already exists"), createdAfterAddErr: streamInfo(nil, jetstream.FileStorage)}
	p, logs := newTestPublisher(js, 1)
	if err := p.EnsureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(js.calls, ",") != "stream_info,add_stream,stream_info" ||
		!strings.Contains(logs.String(), "stream_exists_and_compatible stream=EVENTS") || strings.Contains(logs.String(), "stream_created") {
		t.Errorf("calls=%v logs=%s", js.calls, logs)
	}
}

func TestAddErrorWithoutConfirmedStreamReturnsOriginalError(t *testing.T) {
	addErr := errors.New("stream create failed")
	js := &fakeJetStream{addErr: addErr}
	p, _ := newTestPublisher(js, 1)
	if err := p.EnsureStream(context.Background()); err != addErr || strings.Join(js.calls, ",") != "stream_info,add_stream,stream_info" {
		t.Errorf("err=%v calls=%v", err, js.calls)
	}
}

func TestFollowupInfoFailurePreservesOriginalAddError(t *testing.T) {
	addErr, infoErr := errors.New("create failed"), errors.New("follow-up info denied")
	js := &fakeJetStream{addErr: addErr, followupInfoErr: infoErr}
	p, _ := newTestPublisher(js, 1)
	err := p.EnsureStream(context.Background())
	var wrapped *AddStreamError
	if !errors.As(err, &wrapped) || !errors.Is(err, addErr) || wrapped.InfoErr != infoErr || err.Error() != addErr.Error() {
		t.Errorf("err=%v", err)
	}
	if strings.Join(js.calls, ",") != "stream_info,add_stream,stream_info" {
		t.Errorf("calls %v", js.calls)
	}
}

func TestAddErrorRaceWithIncompatibleStreamKeepsCompatibilityError(t *testing.T) {
	addErr := errors.New("stream name already in use")
	js := &fakeJetStream{addErr: addErr, createdAfterAddErr: streamInfo([]string{"other.>"}, jetstream.FileStorage)}
	p, _ := newTestPublisher(js, 1)
	err := p.EnsureStream(context.Background())
	var compat *CompatibilityError
	if !errors.As(err, &compat) || !strings.Contains(err.Error(), "incompatible subjects") || compat.Cause != addErr {
		t.Errorf("err=%v", err)
	}
}

// racingJetStream makes both initial lookups miss before either add runs,
// then lets both adds succeed idempotently, as the server does for
// identical configurations.
type racingJetStream struct {
	initial   sync.WaitGroup
	present   atomic.Bool
	infoCalls atomic.Int32
	addCalls  atomic.Int32
}

func (r *racingJetStream) StreamInfo(context.Context, string) (*jetstream.StreamConfig, error) {
	r.infoCalls.Add(1)
	r.initial.Done()
	r.initial.Wait()
	return nil, jetstream.ErrStreamNotFound
}

func (r *racingJetStream) AddStream(_ context.Context, cfg jetstream.StreamConfig) (*jetstream.StreamConfig, error) {
	r.addCalls.Add(1)
	r.present.Store(true)
	return &cfg, nil
}

func (r *racingJetStream) Publish(context.Context, string, []byte, string) error { return nil }

func TestTwoWorkersStartingAtOnceBothConnect(t *testing.T) {
	js := &racingJetStream{}
	js.initial.Add(2)
	first, logs1 := newTestPublisher(js, 1)
	second, logs2 := newTestPublisher(js, 1)
	var wg sync.WaitGroup
	for _, p := range []*Publisher{first, second} {
		wg.Add(1)
		go func(p *Publisher) {
			defer wg.Done()
			if err := p.EnsureStream(context.Background()); err != nil {
				t.Error(err)
			}
		}(p)
	}
	wg.Wait()
	if !js.present.Load() || js.infoCalls.Load() != 2 || js.addCalls.Load() != 2 {
		t.Errorf("present=%v info=%d add=%d", js.present.Load(), js.infoCalls.Load(), js.addCalls.Load())
	}
	for _, l := range []*bytes.Buffer{logs1, logs2} {
		if !strings.Contains(l.String(), "stream_created stream=EVENTS") {
			t.Errorf("logs %s", l)
		}
	}
}

func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		context.DeadlineExceeded:                               "TimeoutError",
		nats.ErrTimeout:                                        "TimeoutError",
		jetstream.ErrNoStreamResponse:                          "NoStreamResponseError",
		nats.ErrConnectionClosed:                               "ConnectionClosedError",
		&jetstream.APIError{Code: 503}:                         "ServiceUnavailableError",
		fmt.Errorf("nats: %w", &jetstream.APIError{Code: 400}): "BadRequestError",
		&jetstream.APIError{Code: 409}:                         "APIError",
		errors.New("other"):                                    "Error",
	} {
		if got := ErrorClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}

func TestRedactedURL(t *testing.T) {
	for in, want := range map[string]string{
		"nats://user:secret@NATS.example:4222": "nats://nats.example:4222",
		"nats://localhost":                     "nats://localhost",
		"tls://[::1]:4222":                     "tls://::1:4222",
		"nats://h:0":                           "nats://h",
	} {
		if got := RedactedURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
