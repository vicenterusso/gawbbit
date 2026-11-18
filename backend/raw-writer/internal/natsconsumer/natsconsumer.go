// Package natsconsumer pulls raw events from the JetStream durable consumer.
package natsconsumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/errclass"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/service"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/transformer"
)

// Connection behaviour of the original nats-py client: 2 s connect timeout, 2 s between
// attempts and 60 reconnect attempts, which also bound the initial connect.
const (
	connectTimeout       = 2 * time.Second
	reconnectWait        = 2 * time.Second
	maxReconnectAttempts = 60
)

// ErrBind is returned when the stream or durable consumer cannot be bound or created.
var ErrBind = errors.New("Failed to bind JetStream consumer. Ensure stream exists and, if migrating from events.* to events.>, recreate durable consumer.")

// Consumer is a pull consumer on NATS_STREAM bound to the NATS_CONSUMER durable.
type Consumer struct {
	settings *settings.Settings
	nc       *nats.Conn
	consumer jetstream.Consumer
}

// New returns an unconnected consumer.
func New(s *settings.Settings) *Consumer {
	return &Consumer{settings: s}
}

func (c *Consumer) subject() string { return c.settings.NATSSubjectPrefix + ".>" }

// Connect dials NATS and binds the durable consumer, creating it only when missing so
// that an operator-managed consumer keeps its configuration.
func (c *Consumer) Connect(ctx context.Context) error {
	s := c.settings
	url, _ := settings.RedactedURL(s.NATSURL)
	logx.Std.Infof("connect_start url=%s stream=%s consumer=%s subject_pattern=%s",
		url, s.NATSStream, s.NATSConsumer, c.subject())
	nc, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.nc = nc
	js, err := jetstream.New(nc)
	if err == nil {
		c.consumer, err = c.bind(ctx, js)
	}
	if err != nil {
		logx.Std.Errorf("connect_failed stream=%s consumer=%s error_class=%s", s.NATSStream, s.NATSConsumer, errclass.Of(err))
		return fmt.Errorf("%w: %v", ErrBind, err)
	}
	logx.Std.Infof("connect_ready stream=%s consumer=%s", s.NATSStream, s.NATSConsumer)
	return nil
}

func (c *Consumer) dial(ctx context.Context) (*nats.Conn, error) {
	for attempt := 0; ; attempt++ {
		nc, err := nats.Connect(c.settings.NATSURL,
			nats.Timeout(connectTimeout),
			nats.ReconnectWait(reconnectWait),
			nats.MaxReconnects(maxReconnectAttempts),
		)
		if err == nil {
			return nc, nil
		}
		logx.Std.Errorf("connect_retry attempt=%d/%d error_class=%s", attempt+1, maxReconnectAttempts+1, errclass.Of(err))
		if attempt >= maxReconnectAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(reconnectWait):
		}
	}
}

func (c *Consumer) bind(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	s := c.settings
	if s.NATSConsumer != "" {
		existing, err := js.Consumer(ctx, s.NATSStream, s.NATSConsumer)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return nil, err
		}
	}
	return js.CreateConsumer(ctx, s.NATSStream, jetstream.ConsumerConfig{
		Name:          s.NATSConsumer,
		Durable:       s.NATSConsumer,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       time.Duration(s.NATSAckWaitSeconds) * time.Second,
		MaxDeliver:    s.NATSMaxDeliver,
		MaxAckPending: s.NATSMaxAckPending,
		FilterSubject: c.subject(),
	})
}

// MaxAckPending returns the consumer's max_ack_pending as configured on the server.
func (c *Consumer) MaxAckPending(ctx context.Context) (int, error) {
	if c.consumer == nil {
		return 0, errors.New("consumer is not connected")
	}
	info, err := c.consumer.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.Config.MaxAckPending, nil
}

// Fetch returns the next batch: whatever is available right away, otherwise what arrives
// within NATS_FETCH_TIMEOUT_SECONDS. A cancelled ctx returns early with no messages;
// messages delivered to the abandoned request are redelivered after Ack Wait.
func (c *Consumer) Fetch(ctx context.Context) ([]service.Message, error) {
	if c.consumer == nil {
		return nil, nil
	}
	batchSize := c.settings.NATSFetchBatch
	msgs, err := c.collect(ctx, func() (jetstream.MessageBatch, error) { return c.consumer.FetchNoWait(batchSize) })
	if err != nil || len(msgs) > 0 || ctx.Err() != nil {
		return msgs, err
	}
	timeout := time.Duration(c.settings.NATSFetchTimeoutSeconds * float64(time.Second))
	return c.collect(ctx, func() (jetstream.MessageBatch, error) {
		return c.consumer.Fetch(batchSize, jetstream.FetchMaxWait(timeout))
	})
}

func (c *Consumer) collect(ctx context.Context, start func() (jetstream.MessageBatch, error)) ([]service.Message, error) {
	batch, err := start()
	if err != nil {
		return nil, err
	}
	var out []service.Message
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case m, ok := <-batch.Messages():
			if !ok {
				return out, c.batchError(batch.Error())
			}
			out = append(out, message{m})
		}
	}
}

// batchError keeps a closed connection fatal (the client gave up reconnecting) and
// treats every other fetch problem as an empty fetch, like a timeout.
func (c *Consumer) batchError(err error) error {
	if err == nil || errors.Is(err, jetstream.ErrNoMessages) {
		return nil
	}
	if errors.Is(err, nats.ErrConnectionClosed) || c.nc.IsClosed() {
		return err
	}
	logx.Std.Warningf("fetch_failed error_class=%s", errclass.Of(err))
	return nil
}

// closeFlushTimeout bounds how long Close waits for pending ACKs to reach the server.
const closeFlushTimeout = 5 * time.Second

// Close flushes pending ACKs/NAKs to the server, then closes the connection.
func (c *Consumer) Close() error {
	if c.nc == nil || !c.nc.IsConnected() {
		return nil
	}
	err := c.nc.FlushTimeout(closeFlushTimeout)
	c.nc.Close()
	return err
}

type message struct{ m jetstream.Msg }

func (m message) Data() []byte { return m.m.Data() }

func (m message) Metadata() (*transformer.Metadata, error) {
	meta, err := m.m.Metadata()
	if err != nil {
		return nil, pyval.ErrNotJSMessage
	}
	return &transformer.Metadata{Stream: meta.Stream, StreamSequence: meta.Sequence.Stream}, nil
}

func (m message) Ack() error  { return m.m.Ack() }
func (m message) Nak() error  { return m.m.Nak() }
func (m message) Term() error { return m.m.Term() }
