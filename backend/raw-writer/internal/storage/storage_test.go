package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
)

type fakeS3 struct {
	failures int
	calls    []*s3.PutObjectInput
	bodies   [][]byte
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.calls = append(f.calls, in)
	body, _ := io.ReadAll(in.Body)
	f.bodies = append(f.bodies, body)
	if len(f.calls) <= f.failures {
		return nil, errors.New("boom secret-detail")
	}
	return &s3.PutObjectOutput{}, nil
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := logx.Std
	logx.Std = logx.New(&buf, logx.Debug)
	t.Cleanup(func() { logx.Std = old })
	return &buf
}

func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := sleep
	sleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleep = old })
	return &slept
}

func TestUploadFileUsesS3Client(t *testing.T) {
	client := &fakeS3{}
	u := NewS3WithClient(client, "events")
	path := "raw/app_id=a/event_date=2026-01-01/hour=00/part.parquet"
	if err := u.Upload(context.Background(), []byte("PAR1"), path); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || *client.calls[0].Bucket != "events" || *client.calls[0].Key != path || string(client.bodies[0]) != "PAR1" {
		t.Fatalf("calls %+v", client.calls)
	}
}

func TestUploadRetriesWithBackoffAndSafeLogs(t *testing.T) {
	logs := captureLogs(t)
	slept := noSleep(t)
	client := &fakeS3{failures: 2}
	if err := NewS3WithClient(client, "events").Upload(context.Background(), []byte("x"), "k"); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 3 || len(*slept) != 2 || (*slept)[0] != 2*time.Second || (*slept)[1] != 4*time.Second {
		t.Fatalf("calls=%d slept=%v", len(client.calls), *slept)
	}
	out := logs.String()
	if !strings.Contains(out, "WARNING service=raw-writer event=upload_retry backend=s3 bucket=events object_path=k attempt=1/3 error_class=errorString") ||
		strings.Contains(out, "secret-detail") {
		t.Fatalf("logs %q", out)
	}
}

func TestUploadGivesUpAfterRetries(t *testing.T) {
	captureLogs(t)
	noSleep(t)
	client := &fakeS3{failures: 10}
	if err := NewS3WithClient(client, "events").Upload(context.Background(), []byte("x"), "k"); err == nil {
		t.Fatal("expected error")
	}
	if len(client.calls) != DefaultRetries {
		t.Fatalf("calls=%d", len(client.calls))
	}
}

func TestFactoryBuildsS3Uploader(t *testing.T) {
	s, err := settings.FromMap(map[string]string{
		"RAW_STORAGE_BACKEND": "s3", "S3_ENDPOINT_URL": "http://seaweedfs:8333", "S3_BUCKET": "events",
		"S3_ACCESS_KEY": "key", "S3_SECRET_KEY": "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if s3u, ok := u.(*S3Uploader); !ok || s3u.bucket != "events" {
		t.Fatalf("uploader %#v", u)
	}
}

func TestFactoryBuildsGCSUploader(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "127.0.0.1:1")
	s, err := settings.FromMap(map[string]string{"RAW_STORAGE_BACKEND": "gcs", "GCS_RAW_BUCKET": "bucket-a"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := u.(*GCSUploader); !ok || g.name != "bucket-a" {
		t.Fatalf("uploader %#v", u)
	}
}

func TestS3RejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"seaweedfs:8333", "ftp://host", "http://", "::"} {
		if _, err := NewS3(S3Config{EndpointURL: endpoint, Bucket: "b"}); err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		}
	}
}

func TestGCSUploadUsesMediaUpload(t *testing.T) {
	var gotPath, gotQuery, gotType string
	var gotBody []byte
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotType = r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)
	u, err := NewGCS(context.Background(), "bucket-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Upload(context.Background(), []byte("PAR1"), "raw/app_id=a b/x.parquet"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/upload/storage/v1/b/bucket-a/o" || gotQuery != "uploadType=media&name=raw%2Fapp_id%3Da%20b%2Fx.parquet" ||
		gotType != "application/octet-stream" || string(gotBody) != "PAR1" {
		t.Fatalf("path=%q query=%q type=%q body=%q", gotPath, gotQuery, gotType, gotBody)
	}

	logs := captureLogs(t)
	noSleep(t)
	status = http.StatusForbidden
	if err := u.Upload(context.Background(), []byte("x"), "k"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(logs.String(), "upload_retry backend=gcs bucket=bucket-a object_path=k attempt=3/3 error_class=Forbidden") {
		t.Fatalf("logs %q", logs.String())
	}
}
