package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/transformer"
)

func testSettings(t *testing.T, overrides map[string]string) *settings.Settings {
	t.Helper()
	env := map[string]string{
		"RAW_STORAGE_BACKEND":        "s3",
		"S3_ENDPOINT_URL":            "http://seaweedfs:8333",
		"S3_BUCKET":                  "events",
		"S3_ACCESS_KEY":              "key",
		"S3_SECRET_KEY":              "secret",
		"RAW_MAX_EVENTS_PER_FILE":    "10000",
		"RAW_MAX_BYTES_PER_FILE":     fmt.Sprint(50 * 1024 * 1024),
		"RAW_FLUSH_INTERVAL_SECONDS": "60",
		"NATS_ACK_WAIT_SECONDS":      "300",
	}
	for k, v := range overrides {
		env[k] = v
	}
	s, err := settings.FromMap(env)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeMsg struct {
	data                     []byte
	meta                     *transformer.Metadata
	metaErr                  error
	acked, naked, terminated int
}

func (m *fakeMsg) Data() []byte                             { return m.data }
func (m *fakeMsg) Metadata() (*transformer.Metadata, error) { return m.meta, m.metaErr }
func (m *fakeMsg) Ack() error                               { m.acked++; return nil }
func (m *fakeMsg) Nak() error                               { m.naked++; return nil }
func (m *fakeMsg) Term() error                              { m.terminated++; return nil }
func (m *fakeMsg) counts() [3]int                           { return [3]int{m.acked, m.naked, m.terminated} }

func newMsg(appID string, index int) *fakeMsg {
	data, _ := json.Marshal(map[string]any{
		"event_id":        fmt.Sprintf("%s-%d", appID, index),
		"app_id":          appID,
		"event_name":      "level_completed",
		"event_timestamp": "2026-01-01T10:00:00Z",
	})
	return &fakeMsg{data: data}
}

type fakeUploader struct {
	mu          sync.Mutex
	failing     map[string]bool
	objectPaths []string
	files       [][]byte
}

func (u *fakeUploader) Upload(_ context.Context, data []byte, objectPath string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for app := range u.failing {
		if strings.Contains(objectPath, "app_id="+app+"/") {
			return errors.New("upload failed")
		}
	}
	u.objectPaths = append(u.objectPaths, objectPath)
	u.files = append(u.files, data)
	return nil
}

type fakeConsumer struct {
	maxAckPending    int
	maxAckPendingErr error
	batches          [][]*fakeMsg
	fetchCalls       int
	fetchesStopping  int
	closed           bool
	onFetch          func()
	isStopping       func() bool
	blockAfterFirst  bool
}

func (c *fakeConsumer) Connect(context.Context) error { return nil }

func (c *fakeConsumer) MaxAckPending(context.Context) (int, error) {
	return c.maxAckPending, c.maxAckPendingErr
}

func (c *fakeConsumer) Fetch(ctx context.Context) ([]Message, error) {
	c.fetchCalls++
	if c.isStopping != nil && c.isStopping() {
		c.fetchesStopping++
	}
	var batch []*fakeMsg
	if len(c.batches) > 0 {
		batch, c.batches = c.batches[0], c.batches[1:]
	}
	if c.onFetch != nil {
		c.onFetch()
	}
	wait := 50 * time.Millisecond // a real fetch blocks up to NATS_FETCH_TIMEOUT_SECONDS
	if c.blockAfterFirst && c.fetchCalls > 1 {
		wait = time.Hour
	}
	select {
	case <-ctx.Done():
		if c.blockAfterFirst && c.fetchCalls > 1 {
			return nil, nil
		}
	case <-time.After(wait):
	}
	out := make([]Message, len(batch))
	for i, m := range batch {
		out[i] = m
	}
	return out, nil
}

func (c *fakeConsumer) Close() error { c.closed = true; return nil }

// interleaved builds messages across apps so no partition fills up first.
func interleaved(sizes [][2]any) []*fakeMsg {
	next := make([]int, len(sizes))
	var out []*fakeMsg
	for more := true; more; {
		more = false
		for i, s := range sizes {
			if next[i] < s[1].(int) {
				out = append(out, newMsg(s[0].(string), next[i]))
				next[i]++
				more = true
			}
		}
	}
	return out
}

func newService(t *testing.T, c *fakeConsumer, u *fakeUploader, overrides map[string]string) *Service {
	t.Helper()
	svc, err := New(testSettings(t, overrides), c, u)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func feed(t *testing.T, svc *Service, msgs []*fakeMsg) {
	t.Helper()
	svc.RefreshThreshold()
	for _, m := range msgs {
		if err := svc.HandleMessage(m); err != nil {
			t.Fatal(err)
		}
	}
}

func captureLogs(t *testing.T, level logx.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := logx.Std
	logx.Std = logx.New(&buf, level)
	t.Cleanup(func() { logx.Std = old })
	return &buf
}

func logLines(buf *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

const poison = "private-poison-payload-never-log"

type labelled struct {
	label string
	event any
}

func malformedEvents() []labelled {
	events := []labelled{{"array", []any{}}, {"null", nil}, {"string", poison}, {"number", 42}}
	for _, field := range []string{"user", "device", "ingest"} {
		for _, v := range []labelled{{"array", []any{poison}}, {"string", poison}, {"number", 42}, {"bool", true}} {
			events = append(events, labelled{field + "_" + v.label, map[string]any{field: v.event, "event_id": poison}})
		}
	}
	return events
}

func malformedMsg(event any, sequence uint64) *fakeMsg {
	data, _ := json.Marshal(event)
	return &fakeMsg{data: data, meta: &transformer.Metadata{Stream: "EVENTS", StreamSequence: sequence}}
}

func assertTerminal(t *testing.T, m *fakeMsg) {
	t.Helper()
	if m.counts() != [3]int{0, 0, 1} {
		t.Fatalf("ack/nak/term = %v, want terminal", m.counts())
	}
}

func assertSafeWarning(t *testing.T, logs *bytes.Buffer, errorClass, sequence string) {
	t.Helper()
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("log lines %q", lines)
	}
	want := fmt.Sprintf(" WARNING service=raw-writer event=message_transform_failed action=term error_class=%s stream_sequence=%s", errorClass, sequence)
	if !strings.HasSuffix(lines[0], want) {
		t.Fatalf("log %q, want suffix %q", lines[0], want)
	}
	if strings.Contains(logs.String(), poison) {
		t.Fatal("poison payload logged")
	}
}

func TestMalformedEventsLeavePreexistingBuffersUnchanged(t *testing.T) {
	for _, tc := range malformedEvents() {
		t.Run(tc.label, func(t *testing.T) {
			uploader := &fakeUploader{}
			svc := newService(t, &fakeConsumer{maxAckPending: 10000}, uploader, nil)
			valid := newMsg("existing", 0)
			if err := svc.HandleMessage(valid); err != nil {
				t.Fatal(err)
			}
			key := svc.order[0]
			state := svc.buffers[key]
			rows, approx := len(state.rows), state.approxBytes
			msg := malformedMsg(tc.event, 123)

			logs := captureLogs(t, logx.Warning)
			if err := svc.HandleMessage(msg); err != nil {
				t.Fatal(err)
			}
			assertTerminal(t, msg)
			assertSafeWarning(t, logs, "AttributeError", "123")
			if svc.BufferedEvents() != 1 || len(svc.order) != 1 || svc.buffers[key] != state ||
				len(state.rows) != rows || len(state.msgs) != 1 || state.msgs[0] != valid || state.approxBytes != approx {
				t.Fatal("pre-existing buffer changed")
			}
			if valid.counts() != [3]int{} || len(uploader.objectPaths) != 0 {
				t.Fatal("valid message touched")
			}
		})
	}
}

func TestMalformedMetadataCannotBreakTerminalWarning(t *testing.T) {
	cases := map[string]*fakeMsg{
		"no metadata":    {data: []byte("[]")},
		"metadata error": {data: []byte("[]"), metaErr: errors.New(poison)},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newService(t, &fakeConsumer{maxAckPending: 10000}, &fakeUploader{}, nil)
			logs := captureLogs(t, logx.Warning)
			if err := svc.HandleMessage(msg); err != nil {
				t.Fatal(err)
			}
			assertTerminal(t, msg)
			assertSafeWarning(t, logs, "AttributeError", "None")
			if svc.BufferedEvents() != 0 || len(svc.buffers) != 0 {
				t.Fatal("buffer changed")
			}
		})
	}
}

type poisonError struct{}

func (poisonError) Error() string { return poison }

func TestExceptionContentIsNeverFormattedOrLogged(t *testing.T) {
	svc := newService(t, &fakeConsumer{maxAckPending: 10000}, &fakeUploader{}, nil)
	calls := 0
	svc.transform = func(event any, msg transformer.MetadataSource, _ func() time.Time) (transformer.PartitionKey, *transformer.Row, error) {
		calls++
		return transformer.PartitionKey{}, nil, poisonError{}
	}
	msg := malformedMsg(map[string]any{"event_id": poison}, 123)
	logs := captureLogs(t, logx.Warning)
	if err := svc.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("transform calls %d", calls)
	}
	assertTerminal(t, msg)
	assertSafeWarning(t, logs, "poisonError", "123")
	if svc.BufferedEvents() != 0 || len(svc.buffers) != 0 {
		t.Fatal("buffer changed")
	}
}

func TestRunContinuesSameBatchAndFlushesOnlyValidParquet(t *testing.T) {
	events := malformedEvents()
	malformed := make([]*fakeMsg, len(events))
	for i, e := range events {
		malformed[i] = malformedMsg(e.event, uint64(i))
	}
	valid := make([]*fakeMsg, len(malformed)+1)
	for i := range valid {
		valid[i] = newMsg("neighbors", i)
	}
	batch := []*fakeMsg{valid[0]}
	for i, bad := range malformed {
		batch = append(batch, bad, valid[i+1])
	}
	uploader := &fakeUploader{}
	consumer := &fakeConsumer{maxAckPending: 10000, batches: [][]*fakeMsg{batch}}
	svc := newService(t, consumer, uploader, nil)
	consumer.onFetch = func() {
		if consumer.fetchCalls == 2 {
			svc.RequestStop()
		}
	}

	logs := captureLogs(t, logx.Warning)
	if err := runWithTimeout(t, svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if consumer.fetchCalls != 2 {
		t.Fatalf("fetch calls %d", consumer.fetchCalls)
	}
	if len(logLines(logs)) != len(malformed) || strings.Contains(logs.String(), poison) {
		t.Fatalf("logs %q", logs.String())
	}
	for _, m := range malformed {
		assertTerminal(t, m)
	}
	if svc.BufferedEvents() != len(valid) || len(svc.buffers) != 1 {
		t.Fatalf("buffered %d partitions %d", svc.BufferedEvents(), len(svc.buffers))
	}
	state := svc.buffers[svc.order[0]]
	approx := 0
	for i, m := range valid {
		approx += len(m.data)
		if state.msgs[i] != m || *state.rows[i].EventID != fmt.Sprintf("neighbors-%d", i) || m.counts() != [3]int{} {
			t.Fatalf("buffer entry %d", i)
		}
	}
	if state.approxBytes != approx || len(uploader.objectPaths) != 0 {
		t.Fatal("unexpected buffer bytes or uploads")
	}

	svc.FlushAll()

	if len(uploader.files) != 1 {
		t.Fatalf("files %d", len(uploader.files))
	}
	columns := readColumns(t, uploader.files[0])
	for i := range valid {
		if columns["event_id"][i] != fmt.Sprintf("neighbors-%d", i) || columns["app_id"][i] != "neighbors" ||
			columns["event_name"][i] != "level_completed" || columns["event_timestamp"][i] != "2026-01-01T10:00:00Z" {
			t.Fatalf("row %d: %v", i, columns)
		}
		if valid[i].counts() != [3]int{1, 0, 0} {
			t.Fatalf("valid %d counts %v", i, valid[i].counts())
		}
	}
	for _, m := range malformed {
		assertTerminal(t, m)
	}
	if svc.BufferedEvents() != 0 || len(svc.buffers) != 0 {
		t.Fatal("buffer not empty")
	}
}

func readColumns(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	reader, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := pqarrow.NewFileReader(reader, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	table, err := fr.ReadTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for c := 0; c < int(table.NumCols()); c++ {
		name := table.Schema().Field(c).Name
		for _, chunk := range table.Column(c).Data().Chunks() {
			if arr, ok := chunk.(*array.String); ok {
				for i := 0; i < arr.Len(); i++ {
					out[name] = append(out[name], arr.Value(i))
				}
			}
		}
	}
	return out
}

func runWithTimeout(t *testing.T, svc *Service, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- svc.Run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestBufferThreshold(t *testing.T) {
	ptr := func(v int) *int { return &v }
	tests := []struct {
		maxAckPending int
		override      *int
		want          int
	}{
		{10000, nil, 8000}, {1000, nil, 800}, {10000, ptr(500), 500}, {10000, ptr(20000), 8000}, {1, nil, 1},
	}
	for _, tt := range tests {
		if got := BufferThreshold(tt.maxAckPending, tt.override); got != tt.want {
			t.Errorf("BufferThreshold(%d, %v) = %d, want %d", tt.maxAckPending, tt.override, got, tt.want)
		}
	}
}

func flushedApps(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, strings.SplitN(strings.SplitN(p, "app_id=", 2)[1], "/", 2)[0])
	}
	return out
}

func TestFlushesBeforeAnyPartitionReachesFileLimit(t *testing.T) {
	uploader := &fakeUploader{}
	svc := newService(t, &fakeConsumer{maxAckPending: 1250}, uploader, nil)
	var sizes [][2]any
	for i := 0; i < 10; i++ {
		sizes = append(sizes, [2]any{fmt.Sprintf("app%d", i), 100})
	}
	msgs := interleaved(sizes)
	feed(t, svc, msgs)

	if svc.threshold != 1000 || len(uploader.objectPaths) == 0 || svc.BufferedEvents() >= svc.threshold/2 {
		t.Fatalf("threshold %d uploads %d buffered %d", svc.threshold, len(uploader.objectPaths), svc.BufferedEvents())
	}
	flushed := 0
	for _, m := range msgs {
		if m.acked > 0 {
			flushed++
			if m.counts() != [3]int{1, 0, 0} {
				t.Fatal("double ack")
			}
		}
	}
	if flushed != 1000-svc.BufferedEvents() {
		t.Fatalf("flushed %d buffered %d", flushed, svc.BufferedEvents())
	}
}

func TestFlushesLargestPartitionsFirst(t *testing.T) {
	uploader := &fakeUploader{}
	svc := newService(t, &fakeConsumer{maxAckPending: 1250}, uploader, nil)
	feed(t, svc, interleaved([][2]any{{"big", 500}, {"mid", 300}, {"small", 150}, {"tiny", 50}}))

	if got := flushedApps(uploader.objectPaths); strings.Join(got, ",") != "big,mid" {
		t.Fatalf("flushed %v", got)
	}
	if svc.BufferedEvents() != 200 || svc.BufferedEvents() >= svc.threshold/2 {
		t.Fatalf("buffered %d", svc.BufferedEvents())
	}
	if len(svc.order) != 2 || svc.order[0].AppID != "small" || svc.order[1].AppID != "tiny" {
		t.Fatalf("remaining %v", svc.order)
	}
}

func TestUploadFailureNaksOnlyThatPartition(t *testing.T) {
	uploader := &fakeUploader{failing: map[string]bool{"big": true}}
	svc := newService(t, &fakeConsumer{maxAckPending: 1250}, uploader, nil)
	captureLogs(t, logx.Info)
	msgs := interleaved([][2]any{{"big", 500}, {"mid", 300}, {"small", 150}, {"tiny", 50}})
	feed(t, svc, msgs)

	total := 0
	for _, m := range msgs {
		switch {
		case bytes.Contains(m.data, []byte(`"app_id":"big"`)):
			if m.counts() != [3]int{0, 1, 0} {
				t.Fatalf("big counts %v", m.counts())
			}
		case bytes.Contains(m.data, []byte(`"app_id":"mid"`)):
			if m.counts() != [3]int{1, 0, 0} {
				t.Fatalf("mid counts %v", m.counts())
			}
		}
	}
	for _, key := range svc.order {
		if key.AppID == "big" {
			t.Fatal("big still buffered")
		}
		total += len(svc.buffers[key].msgs)
	}
	if svc.BufferedEvents() != total {
		t.Fatalf("buffered %d != %d", svc.BufferedEvents(), total)
	}
}

func TestPerPartitionCountFlushStillApplies(t *testing.T) {
	uploader := &fakeUploader{}
	svc := newService(t, &fakeConsumer{maxAckPending: 10000}, uploader, map[string]string{"RAW_MAX_EVENTS_PER_FILE": "100"})
	feed(t, svc, interleaved([][2]any{{"solo", 250}}))
	if len(uploader.objectPaths) != 2 || svc.BufferedEvents() != 50 {
		t.Fatalf("uploads %d buffered %d", len(uploader.objectPaths), svc.BufferedEvents())
	}
}

func TestPerPartitionByteFlush(t *testing.T) {
	uploader := &fakeUploader{}
	msgs := interleaved([][2]any{{"solo", 3}})
	limit := len(msgs[0].data) + len(msgs[1].data)
	svc := newService(t, &fakeConsumer{maxAckPending: 10000}, uploader, map[string]string{"RAW_MAX_BYTES_PER_FILE": fmt.Sprint(limit)})
	feed(t, svc, msgs)
	if len(uploader.objectPaths) != 1 || svc.BufferedEvents() != 1 {
		t.Fatalf("uploads %d buffered %d", len(uploader.objectPaths), svc.BufferedEvents())
	}
}

func TestObjectKeyLayout(t *testing.T) {
	uploader := &fakeUploader{}
	svc := newService(t, &fakeConsumer{maxAckPending: 10000}, uploader, map[string]string{"S3_PREFIX": "/raw/"})
	svc.now = func() time.Time { return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC) }
	feed(t, svc, []*fakeMsg{newMsg("app", 0)})
	svc.FlushAll()
	if len(uploader.objectPaths) != 1 {
		t.Fatal(uploader.objectPaths)
	}
	path := uploader.objectPaths[0]
	prefix := "raw/app_id=app/event_date=2026-01-01/hour=10/part-20260203T040506-"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".parquet") || len(path) != len(prefix)+12+len(".parquet") {
		t.Fatalf("path %q", path)
	}
	if strings.Trim(path[len(prefix):len(prefix)+12], "0123456789abcdef") != "" {
		t.Fatalf("random part %q", path)
	}
}

func TestThresholdComesFromConsumerAndOverrideOnlyLowersIt(t *testing.T) {
	tests := []struct {
		maxAckPending int
		override      map[string]string
		want          int
	}{
		{10000, nil, 8000},
		{10000, map[string]string{"RAW_MAX_BUFFERED_EVENTS": "500"}, 500},
		{1000, map[string]string{"RAW_MAX_BUFFERED_EVENTS": "5000"}, 800},
	}
	for _, tt := range tests {
		svc := newService(t, &fakeConsumer{maxAckPending: tt.maxAckPending}, &fakeUploader{}, tt.override)
		svc.RefreshThreshold()
		if svc.threshold != tt.want {
			t.Errorf("threshold %d, want %d", svc.threshold, tt.want)
		}
	}
}

func TestThresholdFollowsConsumerChanges(t *testing.T) {
	consumer := &fakeConsumer{maxAckPending: 10000}
	svc := newService(t, consumer, &fakeUploader{}, nil)
	svc.RefreshThreshold()
	consumer.maxAckPending = 1000
	svc.RefreshThreshold()
	if svc.threshold != 800 {
		t.Fatal(svc.threshold)
	}
}

func TestFailedRefreshKeepsLastKnownThreshold(t *testing.T) {
	consumer := &fakeConsumer{maxAckPending: 5000}
	svc := newService(t, consumer, &fakeUploader{}, nil)
	svc.RefreshThreshold()
	consumer.maxAckPendingErr = errors.New("nats unavailable")
	logs := captureLogs(t, logx.Warning)
	svc.RefreshThreshold()
	if svc.threshold != 4000 || !strings.Contains(logs.String(), "WARNING service=raw-writer event=consumer_info_failed action=keep_threshold threshold=4000") {
		t.Fatalf("threshold %d logs %q", svc.threshold, logs.String())
	}
}

func TestUnlimitedConsumerFallsBackToSetting(t *testing.T) {
	svc := newService(t, &fakeConsumer{maxAckPending: -1}, &fakeUploader{}, map[string]string{"NATS_MAX_ACK_PENDING": "2000"})
	svc.RefreshThreshold()
	if svc.threshold != 1600 {
		t.Fatal(svc.threshold)
	}
}

func TestTimeBasedFlush(t *testing.T) {
	uploader := &fakeUploader{}
	msgs := interleaved([][2]any{{"a", 2}, {"b", 1}})
	consumer := &fakeConsumer{maxAckPending: 10000, batches: [][]*fakeMsg{msgs}}
	svc := newService(t, consumer, uploader, nil)
	start := time.Now()
	svc.now = func() time.Time {
		if consumer.fetchCalls >= 1 {
			return start.Add(61 * time.Second)
		}
		return start
	}
	svc.lastFlush = start
	consumer.onFetch = func() {
		if consumer.fetchCalls == 2 {
			svc.RequestStop()
		}
	}
	if err := runWithTimeout(t, svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if strings.Join(flushedApps(uploader.objectPaths), ",") != "a,b" {
		t.Fatalf("flushed %v", uploader.objectPaths)
	}
}

func runUntilSigterm(t *testing.T, uploader *fakeUploader) (*fakeConsumer, []*fakeMsg) {
	t.Helper()
	msgs := interleaved([][2]any{{"app0", 30}, {"app1", 20}})
	consumer := &fakeConsumer{maxAckPending: 10000, batches: [][]*fakeMsg{msgs}}
	svc := newService(t, consumer, uploader, nil)
	release := HandleSignals(svc)
	defer release()
	consumer.isStopping = svc.stopping
	// Signal while waiting on the next fetch, with the first batch already buffered.
	consumer.onFetch = func() {
		if consumer.fetchCalls == 2 {
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}
	}
	if err := runWithTimeout(t, svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	return consumer, msgs
}

func TestSigtermFlushesAndAcksBuffer(t *testing.T) {
	uploader := &fakeUploader{}
	consumer, msgs := runUntilSigterm(t, uploader)
	if consumer.fetchesStopping != 0 || !consumer.closed || len(uploader.objectPaths) != 2 {
		t.Fatalf("fetches while stopping %d closed %v uploads %d", consumer.fetchesStopping, consumer.closed, len(uploader.objectPaths))
	}
	for _, m := range msgs {
		if m.acked != 1 {
			t.Fatal("message not acked")
		}
	}
}

func TestSigtermNaksBufferWhenUploadFails(t *testing.T) {
	captureLogs(t, logx.Info)
	consumer, msgs := runUntilSigterm(t, &fakeUploader{failing: map[string]bool{"app0": true, "app1": true}})
	if consumer.fetchesStopping != 0 {
		t.Fatal("fetched while stopping")
	}
	for _, m := range msgs {
		if m.counts() != [3]int{0, 1, 0} {
			t.Fatalf("counts %v", m.counts())
		}
	}
}

func TestStopDoesNotWaitForABlockedFetch(t *testing.T) {
	msgs := interleaved([][2]any{{"app0", 10}})
	consumer := &fakeConsumer{maxAckPending: 10000, batches: [][]*fakeMsg{msgs}, blockAfterFirst: true}
	uploader := &fakeUploader{}
	svc := newService(t, consumer, uploader, nil)
	time.AfterFunc(100*time.Millisecond, svc.RequestStop)
	if err := runWithTimeout(t, svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if consumer.fetchCalls != 2 || len(uploader.objectPaths) != 1 {
		t.Fatalf("fetch calls %d uploads %d", consumer.fetchCalls, len(uploader.objectPaths))
	}
	for _, m := range msgs {
		if m.acked != 1 {
			t.Fatal("not acked")
		}
	}
}

func TestShutdownLogsAndCloseIsIdempotent(t *testing.T) {
	logs := captureLogs(t, logx.Info)
	consumer := &fakeConsumer{maxAckPending: 10000}
	svc := newService(t, consumer, &fakeUploader{}, nil)
	svc.RequestStop()
	svc.RequestStop()
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Count(out, "shutdown_requested buffered_events=0") != 1 || strings.Count(out, "shutdown_complete") != 1 {
		t.Fatalf("logs %q", out)
	}
}

func TestStartupLog(t *testing.T) {
	logs := captureLogs(t, logx.Info)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	newService(t, &fakeConsumer{}, &fakeUploader{}, map[string]string{"NATS_URL": "nats://user:pw@NATS:4222/x"})
	want := "INFO service=raw-writer event=startup nats_url=nats://nats:4222 stream=EVENTS consumer=raw-writer storage_backend=s3 " +
		"storage_bucket=events storage_prefix=raw flush_interval_seconds=60 max_events_per_file=10000 max_bytes_per_file=52428800 " +
		"max_buffered_events_override=None google_credentials_path_set=False"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("logs %q", logs.String())
	}
}
