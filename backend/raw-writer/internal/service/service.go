// Package service is the raw-writer loop: fetch events, buffer them per partition, write
// Parquet files and ACK messages only after their file is stored.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/errclass"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/parquetwriter"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/partitioning"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/storage"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/transformer"
)

// PressureThresholdRatio is the share of the consumer's max_ack_pending the writer may
// hold unACKed before it flushes early.
const PressureThresholdRatio = 0.8

// consumerInfoTimeout bounds the max_ack_pending lookup.
const consumerInfoTimeout = 5 * time.Second

// Message is one delivered JetStream message.
type Message interface {
	transformer.MetadataSource
	Data() []byte
	Ack() error
	Nak() error
	Term() error
}

// Consumer delivers batches of messages.
type Consumer interface {
	Connect(ctx context.Context) error
	// MaxAckPending is the server-side max_ack_pending of the durable consumer.
	MaxAckPending(ctx context.Context) (int, error)
	// Fetch returns the next batch; it returns early with no messages once ctx is done.
	Fetch(ctx context.Context) ([]Message, error)
	Close() error
}

// BufferThreshold is the total buffered events that trigger a pressure flush.
// JetStream stops delivering once max_ack_pending messages are unACKed, so the writer
// must flush before reaching it; RAW_MAX_BUFFERED_EVENTS can only lower the threshold.
func BufferThreshold(maxAckPending int, override *int) int {
	threshold := max(1, int(float64(maxAckPending)*PressureThresholdRatio))
	if override != nil {
		threshold = min(threshold, *override)
	}
	return threshold
}

type bufferState struct {
	rows        []*transformer.Row
	msgs        []Message
	approxBytes int
}

// Service holds the per-partition buffers. All methods except RequestStop must be
// called from the goroutine running Run.
type Service struct {
	settings *settings.Settings
	consumer Consumer
	uploader storage.Uploader

	// Buffers in first-insertion order; flush_all and ties in pressure flushes follow it.
	order   []transformer.PartitionKey
	buffers map[transformer.PartitionKey]*bufferState

	bufferedEvents        atomic.Int64
	consumerMaxAckPending int
	threshold             int
	lastFlush             time.Time

	stopOnce sync.Once
	stopCtx  context.Context
	stop     context.CancelFunc
	closed   bool

	now       func() time.Time
	transform func(event any, msg transformer.MetadataSource, now func() time.Time) (transformer.PartitionKey, *transformer.Row, error)
}

// New builds the service and logs the startup configuration.
func New(s *settings.Settings, consumer Consumer, uploader storage.Uploader) (*Service, error) {
	natsURL, err := settings.RedactedURL(s.NATSURL)
	if err != nil {
		return nil, err
	}
	stopCtx, stop := context.WithCancel(context.Background())
	svc := &Service{
		settings:  s,
		consumer:  consumer,
		uploader:  uploader,
		buffers:   map[transformer.PartitionKey]*bufferState{},
		threshold: BufferThreshold(s.NATSMaxAckPending, s.RawMaxBufferedEvents),
		stopCtx:   stopCtx,
		stop:      stop,
		now:       time.Now,
		transform: transformer.EventToRow,
	}
	svc.lastFlush = svc.now()
	logx.Std.Infof("startup nats_url=%s stream=%s consumer=%s storage_backend=%s storage_bucket=%s storage_prefix=%s "+
		"flush_interval_seconds=%d max_events_per_file=%d max_bytes_per_file=%d max_buffered_events_override=%s google_credentials_path_set=%s",
		natsURL, s.NATSStream, s.NATSConsumer, s.RawStorageBackend, s.StorageBucket(), s.StoragePrefix(),
		s.RawFlushIntervalSeconds, s.RawMaxEventsPerFile, s.RawMaxBytesPerFile, logx.Py(s.RawMaxBufferedEvents),
		logx.Py(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != ""))
	return svc, nil
}

// RequestStop asks Run to return after the current batch; it is safe from any goroutine.
func (s *Service) RequestStop() {
	s.stopOnce.Do(func() {
		logx.Std.Infof("shutdown_requested buffered_events=%d", s.bufferedEvents.Load())
		s.stop()
	})
}

func (s *Service) stopping() bool { return s.stopCtx.Err() != nil }

// BufferedEvents is the number of messages held unACKed.
func (s *Service) BufferedEvents() int { return int(s.bufferedEvents.Load()) }

// RefreshThreshold re-reads max_ack_pending from the server, keeping the last known
// threshold when that fails.
func (s *Service) RefreshThreshold() {
	ctx, cancel := context.WithTimeout(context.Background(), consumerInfoTimeout)
	defer cancel()
	maxAckPending, err := s.consumer.MaxAckPending(ctx)
	if err != nil {
		logx.Std.Warningf("consumer_info_failed action=keep_threshold threshold=%d error_class=%s", s.threshold, errclass.Of(err))
		return
	}
	if maxAckPending <= 0 {
		// -1 means unlimited on the server; keep memory bounded by the configured value.
		maxAckPending = s.settings.NATSMaxAckPending
	}
	if maxAckPending == s.consumerMaxAckPending {
		return
	}
	if maxAckPending != s.settings.NATSMaxAckPending {
		logx.Std.Warningf("consumer_max_ack_pending_differs consumer_max_ack_pending=%d nats_max_ack_pending_setting=%d",
			maxAckPending, s.settings.NATSMaxAckPending)
	}
	s.consumerMaxAckPending = maxAckPending
	s.threshold = BufferThreshold(maxAckPending, s.settings.RawMaxBufferedEvents)
	logx.Std.Infof("buffer_threshold threshold=%d consumer_max_ack_pending=%d max_buffered_events_override=%s",
		s.threshold, maxAckPending, logx.Py(s.settings.RawMaxBufferedEvents))
}

// Run connects and processes batches until RequestStop. Messages already fetched when
// the stop arrives are still handled; no fetch starts afterwards.
func (s *Service) Run() error {
	if err := s.consumer.Connect(s.stopCtx); err != nil {
		if s.stopping() && errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	s.RefreshThreshold()
	logx.Std.Infof("consumer_ready threshold=%d", s.threshold)

	interval := time.Duration(s.settings.RawFlushIntervalSeconds) * time.Second
	for !s.stopping() {
		messages, err := s.consumer.Fetch(s.stopCtx)
		if err != nil {
			return err
		}
		for _, msg := range messages {
			if err := s.HandleMessage(msg); err != nil {
				return err
			}
		}
		if s.now().Sub(s.lastFlush) >= interval {
			s.FlushAll()
			s.RefreshThreshold()
		}
	}
	return nil
}

// HandleMessage buffers one message or TERMs it when it cannot become a row. The
// warning carries only the error class and stream sequence, never payload content.
func (s *Service) HandleMessage(msg Message) error {
	data := msg.Data()
	event, err := pyval.Loads(data)
	if err != nil {
		logx.Std.Warningf("message_decode_failed action=term")
		return msg.Term()
	}
	key, row, err := s.transform(event, msg, s.now)
	if err != nil {
		sequence := "None"
		if meta, metaErr := msg.Metadata(); metaErr == nil && meta != nil {
			sequence = fmt.Sprint(meta.StreamSequence)
		}
		logx.Std.Warningf("message_transform_failed action=term error_class=%s stream_sequence=%s", errclass.Of(err), sequence)
		return msg.Term()
	}

	buf, ok := s.buffers[key]
	if !ok {
		buf = &bufferState{}
		s.buffers[key] = buf
		s.order = append(s.order, key)
	}
	buf.rows = append(buf.rows, row)
	buf.msgs = append(buf.msgs, msg)
	buf.approxBytes += len(data)
	s.bufferedEvents.Add(1)

	if len(buf.rows) >= s.settings.RawMaxEventsPerFile || buf.approxBytes >= s.settings.RawMaxBytesPerFile {
		s.flushPartition(key, buf)
	}
	if s.BufferedEvents() >= s.threshold {
		s.FlushUnderPressure()
	}
	return nil
}

// FlushUnderPressure flushes the largest partitions until the buffer is below half the
// threshold. Largest-first keeps files large while freeing ACK slots quickly; every
// flushed partition leaves the buffer (ACKed or NAKed), so the loop always progresses.
func (s *Service) FlushUnderPressure() {
	lowWater := s.threshold / 2
	before := s.BufferedEvents()
	keys := append([]transformer.PartitionKey(nil), s.order...)
	sort.SliceStable(keys, func(i, j int) bool { return len(s.buffers[keys[i]].msgs) > len(s.buffers[keys[j]].msgs) })
	flushed := 0
	for _, key := range keys {
		if s.BufferedEvents() < lowWater {
			break
		}
		s.flushPartition(key, s.buffers[key])
		flushed++
	}
	logx.Std.Infof("pressure_flush threshold=%d buffered_before=%d buffered_after=%d flushed_partitions=%d",
		s.threshold, before, s.BufferedEvents(), flushed)
}

func (s *Service) release(key transformer.PartitionKey, state *bufferState) {
	if s.buffers[key] == state {
		delete(s.buffers, key)
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.bufferedEvents.Add(-int64(len(state.msgs)))
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func safeToken(v string) string {
	return strings.Map(func(r rune) rune {
		if !pyval.IsPrintable(r) || r == '\r' || r == '\n' || r == '\t' {
			return '_'
		}
		return r
	}, v)
}

// flushPartition writes one partition's rows to a file, ACKs its messages on success
// and NAKs them on failure; either way the partition leaves the buffer.
func (s *Service) flushPartition(key transformer.PartitionKey, state *bufferState) {
	defer s.release(key, state)
	if len(state.rows) == 0 {
		return
	}
	appIDSafe := safeToken(key.AppID)
	logx.Std.Debugf("flush_start app_id=%s event_date=%s hour=%s rows=%d approx_bytes=%d",
		appIDSafe, key.EventDate, key.Hour, len(state.rows), state.approxBytes)
	filename := "part-" + s.now().UTC().Format("20060102T150405") + "-" + randomHex(12) + ".parquet"
	objectPath := partitioning.ObjectPartitionPath(s.settings.StoragePrefix(), key.AppID, key.EventDate, key.Hour, filename)

	parquetBytes, err := s.writeAndAck(state, objectPath)
	if err != nil {
		for _, msg := range state.msgs {
			_ = msg.Nak()
		}
		logx.Std.Errorf("flush_failed app_id=%s event_date=%s hour=%s rows=%d naks=%d error_class=%s",
			appIDSafe, key.EventDate, key.Hour, len(state.rows), len(state.msgs), errclass.Of(err))
		return
	}
	logx.Std.Infof("flush_success app_id=%s event_date=%s hour=%s rows=%d parquet_bytes=%d acked_messages=%d storage_backend=%s object_path=%s",
		appIDSafe, key.EventDate, key.Hour, len(state.rows), parquetBytes, len(state.msgs), s.settings.RawStorageBackend, objectPath)
}

func (s *Service) writeAndAck(state *bufferState, objectPath string) (int, error) {
	data, err := parquetwriter.WriteRows(state.rows)
	if err != nil {
		return 0, err
	}
	// Uploads are not tied to the stop signal: the shutdown flush must still land.
	if err := s.uploader.Upload(context.Background(), data, objectPath); err != nil {
		return 0, err
	}
	for _, msg := range state.msgs {
		if err := msg.Ack(); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

// FlushAll flushes every partition in insertion order.
func (s *Service) FlushAll() {
	for _, key := range append([]transformer.PartitionKey(nil), s.order...) {
		s.flushPartition(key, s.buffers[key])
	}
	s.lastFlush = s.now()
}

// Close flushes what is buffered and closes the consumer; later calls do nothing.
func (s *Service) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.FlushAll()
	if err := s.consumer.Close(); err != nil {
		return err
	}
	logx.Std.Infof("shutdown_complete")
	return nil
}

// HandleSignals calls RequestStop on SIGTERM and SIGINT until the returned function is
// called.
func HandleSignals(s *Service) (release func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				s.RequestStop()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
