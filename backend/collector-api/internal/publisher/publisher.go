// Package publisher hands accepted events to NATS JetStream.
package publisher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// Connection defaults of the original nats-py client.
const (
	connectTimeout       = 2 * time.Second
	reconnectWait        = 2 * time.Second
	maxReconnectAttempts = 60
	pingInterval         = 120 * time.Second
	maxPingsOutstanding  = 2
	drainTimeout         = 30 * time.Second
	// publishTimeout is nats-py's default JetStream request timeout.
	publishTimeout = 5 * time.Second
)

// Message is one event to publish.
type Message struct {
	Subject string
	Payload []byte
	MsgID   string
}

// BatchPublishError reports a batch in which at least one publish failed.
type BatchPublishError struct {
	Published, Failed int
	FirstError        error
}

func (e *BatchPublishError) Error() string {
	return fmt.Sprintf("%d of %d events failed to publish", e.Failed, e.Published+e.Failed)
}

// JetStream is the subset of JetStream operations the publisher needs;
// tests substitute fakes.
type JetStream interface {
	StreamInfo(ctx context.Context, name string) (*jetstream.StreamConfig, error)
	AddStream(ctx context.Context, cfg jetstream.StreamConfig) (*jetstream.StreamConfig, error)
	Publish(ctx context.Context, subject string, payload []byte, msgID string) error
}

// Config configures a Publisher.
type Config struct {
	URL                    string
	Stream                 string
	SubjectPrefix          string
	StreamMaxAgeSeconds    int64
	DuplicateWindowSeconds int64
	MaxInflightPublishes   int64
}

// Publisher publishes event batches concurrently with a process-wide cap on
// publishes awaiting an ACK.
type Publisher struct {
	cfg      Config
	log      *logx.Logger
	inflight chan struct{}

	nc *nats.Conn
	js JetStream
}

// maxInflightCap bounds the semaphore allocation; far above any reachable
// concurrency, so larger settings behave the same.
const maxInflightCap = 1 << 20

// New returns an unconnected Publisher.
func New(cfg Config, log *logx.Logger) *Publisher {
	limit := cfg.MaxInflightPublishes
	if limit > maxInflightCap {
		limit = maxInflightCap
	}
	return &Publisher{cfg: cfg, log: log, inflight: make(chan struct{}, limit)}
}

// NewWithJetStream returns a Publisher using js directly (for tests).
func NewWithJetStream(cfg Config, log *logx.Logger, js JetStream) *Publisher {
	p := New(cfg, log)
	p.js = js
	return p
}

// RedactedURL keeps only scheme, host and port of a URL for logging.
func RedactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "://"
	}
	port := ""
	if n, err := strconv.Atoi(u.Port()); err == nil && n > 0 && n <= 65535 {
		port = ":" + strconv.Itoa(n)
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + port
}

func (p *Publisher) subjectPattern() string { return p.cfg.SubjectPrefix + ".>" }

// Connect connects to NATS (retrying like nats-py: up to 61 attempts, 2 s
// apart) and makes sure the stream exists and is compatible.
func (p *Publisher) Connect(ctx context.Context) error {
	p.log.Info("nats_connect_start url=%s stream=%s subject_pattern=%s",
		RedactedURL(p.cfg.URL), p.cfg.Stream, p.subjectPattern())
	opts := []nats.Option{
		nats.Timeout(connectTimeout),
		nats.ReconnectWait(reconnectWait),
		nats.MaxReconnects(maxReconnectAttempts),
		nats.PingInterval(pingInterval),
		nats.MaxPingsOutstanding(maxPingsOutstanding),
		nats.DrainTimeout(drainTimeout),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			p.log.Error("nats: encountered error error=%v", err)
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				p.log.Error("nats: encountered error error=%v", err)
			}
		}),
	}
	var nc *nats.Conn
	var err error
	for attempt := 0; ; attempt++ {
		nc, err = nats.Connect(p.cfg.URL, opts...)
		if err == nil {
			break
		}
		p.log.Error("nats: encountered error error=%v", err)
		if attempt >= maxReconnectAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectWait):
		}
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return err
	}
	p.nc = nc
	p.js = natsJetStream{js: js}
	if err := p.EnsureStream(ctx); err != nil {
		return err
	}
	p.log.Info("nats_connect_ready stream=%s", p.cfg.Stream)
	return nil
}

func (p *Publisher) streamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:       p.cfg.Stream,
		Subjects:   []string{p.subjectPattern()},
		Storage:    jetstream.FileStorage,
		MaxAge:     time.Duration(p.cfg.StreamMaxAgeSeconds) * time.Second,
		Duplicates: time.Duration(p.cfg.DuplicateWindowSeconds) * time.Second,
	}
}

// CompatibilityError reports an existing stream that does not fit.
type CompatibilityError struct {
	msg   string
	Cause error // the add_stream error that preceded the check, if any
}

func (e *CompatibilityError) Error() string { return e.msg }
func (e *CompatibilityError) Unwrap() error { return e.Cause }

// AddStreamError is an add-stream failure whose follow-up lookup failed too.
type AddStreamError struct {
	Err, InfoErr error
}

func (e *AddStreamError) Error() string { return e.Err.Error() }
func (e *AddStreamError) Unwrap() error { return e.Err }

// EnsureStream checks for an existing compatible stream before adding one.
// A failed add is followed by another lookup, so that a stream created
// concurrently by another collector is accepted.
func (p *Publisher) EnsureStream(ctx context.Context) error {
	cfg := p.streamConfig()
	info, err := p.js.StreamInfo(ctx, p.cfg.Stream)
	if err == nil {
		subjects, err := p.validate(info, nil)
		if err != nil {
			return err
		}
		p.log.Info("stream_exists_and_compatible stream=%s subjects=%s", p.cfg.Stream, subjects)
		return nil
	}
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return err
	}

	created, addErr := p.js.AddStream(ctx, cfg)
	if addErr != nil {
		info, infoErr := p.js.StreamInfo(ctx, p.cfg.Stream)
		if errors.Is(infoErr, jetstream.ErrStreamNotFound) {
			return addErr
		}
		if infoErr != nil {
			return &AddStreamError{Err: addErr, InfoErr: infoErr}
		}
		subjects, err := p.validate(info, addErr)
		if err != nil {
			return err
		}
		p.log.Info("stream_exists_and_compatible stream=%s subjects=%s", p.cfg.Stream, subjects)
		return nil
	}
	if _, err := p.validate(created, nil); err != nil {
		return err
	}
	p.log.Info("stream_created stream=%s", p.cfg.Stream)
	return nil
}

// validate returns the stream's sorted subjects rendered as a Python list.
func (p *Publisher) validate(info *jetstream.StreamConfig, cause error) (string, error) {
	unique := map[string]bool{}
	for _, s := range info.Subjects {
		unique[s] = true
	}
	subjects := make([]string, 0, len(unique))
	for s := range unique {
		subjects = append(subjects, s)
	}
	sort.Strings(subjects)
	quoted := make([]string, len(subjects))
	for i, s := range subjects {
		quoted[i] = pyjson.Repr(s)
	}
	rendered := "[" + strings.Join(quoted, ", ") + "]"

	expected := p.subjectPattern()
	if !unique[expected] {
		return "", &CompatibilityError{
			msg: fmt.Sprintf("JetStream stream '%s' exists with incompatible subjects %s; expected to include '%s'",
				p.cfg.Stream, rendered, expected),
			Cause: cause,
		}
	}
	if info.Storage != jetstream.FileStorage {
		return "", &CompatibilityError{
			msg:   fmt.Sprintf("JetStream stream '%s' uses storage '%s', expected FILE", p.cfg.Stream, storageName(info.Storage)),
			Cause: cause,
		}
	}
	return rendered, nil
}

// storageName is the storage type as the server reports it.
func storageName(s jetstream.StorageType) string {
	if s == jetstream.MemoryStorage {
		return "memory"
	}
	return "file"
}

// Publish publishes one message and waits for its ACK. The Nats-Msg-Id
// header lets JetStream drop duplicates of retried batches.
func (p *Publisher) Publish(ctx context.Context, m Message) error {
	p.inflight <- struct{}{}
	defer func() { <-p.inflight }()
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	return p.js.Publish(ctx, m.Subject, m.Payload, pytext.Strip(m.MsgID))
}

// PublishBatch publishes every message concurrently and waits for all ACKs.
// Order is not preserved. Every message is attempted; if any fails, a
// *BatchPublishError carries the counts and the first failure in batch order.
func (p *Publisher) PublishBatch(ctx context.Context, messages []Message) error {
	errs := make([]error, len(messages))
	var wg sync.WaitGroup
	for i := range messages {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = p.Publish(ctx, messages[i])
		}(i)
	}
	wg.Wait()
	failed := 0
	var first error
	for _, err := range errs {
		if err != nil {
			if first == nil {
				first = err
			}
			failed++
		}
	}
	if failed > 0 {
		return &BatchPublishError{Published: len(messages) - failed, Failed: failed, FirstError: first}
	}
	return nil
}

// Close drains the connection if it is connected.
func (p *Publisher) Close() {
	if p.nc == nil || !p.nc.IsConnected() {
		return
	}
	closed := make(chan struct{})
	p.nc.SetClosedHandler(func(*nats.Conn) { close(closed) })
	if err := p.nc.Drain(); err != nil {
		p.nc.Close()
		return
	}
	select {
	case <-closed:
	case <-time.After(drainTimeout + time.Second):
		p.nc.Close()
	}
}

// ErrorClass names an error the way the Python service logged it
// (the nats-py exception class).
func ErrorClass(err error) string {
	var apiErr *jetstream.APIError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return "TimeoutError"
	case errors.Is(err, jetstream.ErrNoStreamResponse), errors.Is(err, nats.ErrNoResponders):
		return "NoStreamResponseError"
	case errors.Is(err, nats.ErrConnectionClosed):
		return "ConnectionClosedError"
	case errors.Is(err, nats.ErrConnectionDraining):
		return "ConnectionDrainingError"
	case errors.As(err, &apiErr):
		switch apiErr.Code {
		case 503:
			return "ServiceUnavailableError"
		case 500:
			return "ServerError"
		case 404:
			return "NotFoundError"
		case 400:
			return "BadRequestError"
		}
		return "APIError"
	}
	return "Error"
}

// natsJetStream adapts jetstream.JetStream to JetStream.
type natsJetStream struct{ js jetstream.JetStream }

func (n natsJetStream) StreamInfo(ctx context.Context, name string) (*jetstream.StreamConfig, error) {
	s, err := n.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg := s.CachedInfo().Config
	return &cfg, nil
}

func (n natsJetStream) AddStream(ctx context.Context, cfg jetstream.StreamConfig) (*jetstream.StreamConfig, error) {
	s, err := n.js.CreateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	created := s.CachedInfo().Config
	return &created, nil
}

func (n natsJetStream) Publish(ctx context.Context, subject string, payload []byte, msgID string) error {
	msg := &nats.Msg{Subject: subject, Data: payload, Header: nats.Header{"Nats-Msg-Id": []string{msgID}}}
	_, err := n.js.PublishMsg(ctx, msg, jetstream.WithRetryAttempts(0))
	return err
}
