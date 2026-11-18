package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/config"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/geoip"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/publisher"
)

// The testdata/*.jsonl.gz files were recorded by running the original Python
// collector (uvicorn + FastAPI) against a real NATS JetStream and capturing,
// for every request of a generated corpus, the HTTP response and the stored
// JetStream messages (raw headers and payload). Request IDs and receive
// times are normalized. These tests replay the corpus against the Go handler.

type goldenMessage struct {
	Subject string `json:"subject"`
	Headers string `json:"headers"`
	Data    string `json:"data"`
}

type goldenResult struct {
	Response struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	} `json:"response"`
	Messages []goldenMessage `json:"messages"`
}

type goldenCase struct {
	Name     string      `json:"name"`
	Method   string      `json:"method"`
	Target   string      `json:"target"`
	Headers  [][2]string `json:"headers"`
	Body     string      `json:"body"`
	Expected goldenResult
}

// recordingJetStream stores messages the way JetStream does: headers as
// nats.go serializes them, deduplicated on Nats-Msg-Id.
type recordingJetStream struct {
	mu     sync.Mutex
	seen   map[string]bool
	stored []goldenMessage
}

func (r *recordingJetStream) StreamInfo(context.Context, string) (*jetstream.StreamConfig, error) {
	return nil, jetstream.ErrStreamNotFound
}

func (r *recordingJetStream) AddStream(_ context.Context, cfg jetstream.StreamConfig) (*jetstream.StreamConfig, error) {
	return &cfg, nil
}

func (r *recordingJetStream) Publish(_ context.Context, subject string, payload []byte, msgID string) error {
	var hdr bytes.Buffer
	hdr.WriteString("NATS/1.0\r\n")
	_ = http.Header{"Nats-Msg-Id": {msgID}}.Write(&hdr)
	hdr.WriteString("\r\n")
	id := strings.TrimPrefix(strings.SplitN(hdr.String(), "\r\n", 3)[1], "Nats-Msg-Id: ")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[id] {
		return nil
	}
	r.seen[id] = true
	r.stored = append(r.stored, goldenMessage{Subject: subject, Headers: hdr.String(), Data: string(payload)})
	return nil
}

func (r *recordingJetStream) take() []goldenMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.stored
	r.stored = nil
	return out
}

var (
	goldenUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)
	goldenNow  = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?\+00:00`)
)

const goldenPrefix = "goldenprefix"

func latin1Text(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// sanitizeHeaders turns CR/LF inside header values into spaces, which is
// what nats.go does where nats-py wrote them raw (a documented deviation).
func sanitizeHeaders(h string) string {
	const head, tail = "NATS/1.0\r\n", "\r\n\r\n"
	if !strings.HasPrefix(h, head) || !strings.HasSuffix(h, tail) {
		return h
	}
	inner := h[len(head) : len(h)-len(tail)]
	return head + strings.NewReplacer("\r", " ", "\n", " ").Replace(inner) + tail
}

func rawRequest(c goldenCase) []byte {
	dec := func(s string) []byte {
		b, _ := base64.StdEncoding.DecodeString(s)
		return b
	}
	body := dec(c.Body)
	var b bytes.Buffer
	b.WriteString(c.Method + " ")
	b.Write(dec(c.Target))
	b.WriteString(" HTTP/1.1\r\n")
	for _, h := range c.Headers {
		b.Write(dec(h[0]))
		b.WriteString(": ")
		b.Write(dec(h[1]))
		b.WriteString("\r\n")
	}
	if (c.Method != http.MethodGet && c.Method != http.MethodHead) || len(body) > 0 {
		b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	b.WriteString("Connection: close\r\n\r\n")
	b.Write(body)
	return b.Bytes()
}

func normalizeResponse(rec *httptest.ResponseRecorder, method string) (goldenResult, string) {
	var out goldenResult
	res := rec.Result()
	out.Response.Status = res.StatusCode
	out.Response.Headers = map[string][]string{}
	for k, vs := range res.Header {
		k = strings.ToLower(k)
		if k == "date" || k == "server" || k == "connection" {
			continue
		}
		for _, v := range vs {
			if k == "allow" {
				parts := strings.Split(v, ",")
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				sort.Strings(parts)
				v = strings.Join(parts, ", ")
			}
			out.Response.Headers[k] = append(out.Response.Headers[k], latin1Text([]byte(v)))
		}
	}
	body := rec.Body.Bytes()
	if method == http.MethodHead {
		body = nil
	}
	text := latin1Text(body)
	rid := ""
	if res.StatusCode == http.StatusOK && strings.HasPrefix(text, `{"request_id"`) {
		rid = goldenUUID.FindString(text)
		text = goldenUUID.ReplaceAllString(text, "<uuid>")
	}
	out.Response.Body = text
	return out, rid
}

func replayProfile(t *testing.T, path string) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	if !sc.Scan() {
		t.Fatal("empty golden file")
	}
	var header struct {
		Profile string            `json:"profile"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatal(err)
	}
	env := []string{"NATS_SUBJECT_PREFIX=" + goldenPrefix}
	forwarded := "127.0.0.1"
	for k, v := range header.Env {
		switch k {
		case "GEOIP_MMDB_PATH":
			if strings.HasPrefix(v, "/home/") {
				v = filepath.Join("..", "geoip", "testdata", filepath.Base(v))
			}
		case "FORWARDED_ALLOW_IPS":
			forwarded = v
		}
		env = append(env, k+"="+v)
	}
	settings, err := config.Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New(io.Discard, logx.LevelDebug)
	js := &recordingJetStream{seen: map[string]bool{}}
	pub := publisher.NewWithJetStream(publisher.Config{SubjectPrefix: goldenPrefix, MaxInflightPublishes: 512}, log, js)
	resolver := geoip.NewResolver(settings.GeoIPEnabled, settings.GeoIPMMDBPath, nil, log)
	defer resolver.Close()
	srv := New(Options{Settings: settings, Log: log, Publisher: pub, GeoIP: resolver, ForwardedAllowIPs: forwarded})

	n := 0
	for sc.Scan() {
		var c goldenCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		n++
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawRequest(c))))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		req.RemoteAddr = "127.0.0.1:40000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		got, rid := normalizeResponse(rec, c.Method)
		got.Messages = []goldenMessage{}
		for _, m := range js.take() {
			data := latin1Text([]byte(m.Data))
			if rid != "" {
				data = strings.Replace(data, rid, "<uuid>", 1)
			}
			got.Messages = append(got.Messages, goldenMessage{
				Subject: strings.Replace(m.Subject, goldenPrefix, "<prefix>", 1),
				Headers: latin1Text([]byte(m.Headers)),
				Data:    goldenNow.ReplaceAllString(data, "<now>"),
			})
		}
		sort.Slice(got.Messages, func(i, j int) bool {
			a, b := got.Messages[i], got.Messages[j]
			if a.Subject != b.Subject {
				return a.Subject < b.Subject
			}
			if a.Headers != b.Headers {
				return a.Headers < b.Headers
			}
			return a.Data < b.Data
		})
		want := c.Expected
		if want.Messages == nil {
			want.Messages = []goldenMessage{}
		}
		for i := range want.Messages {
			want.Messages[i].Headers = sanitizeHeaders(want.Messages[i].Headers)
		}
		sort.Slice(want.Messages, func(i, j int) bool {
			a, b := want.Messages[i], want.Messages[j]
			if a.Subject != b.Subject {
				return a.Subject < b.Subject
			}
			if a.Headers != b.Headers {
				return a.Headers < b.Headers
			}
			return a.Data < b.Data
		})
		if !reflect.DeepEqual(got, want) {
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(want)
			t.Errorf("%s (%s):\n got %s\nwant %s", header.Profile, c.Name, gj, wj)
		}
	}
	if n == 0 {
		t.Fatal("no cases")
	}
}

func TestHTTPGoldenFromPythonCollector(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.jsonl.gz")
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) { replayProfile(t, f) })
	}
}
