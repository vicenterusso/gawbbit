//go:build integration

// Integration test against real NATS JetStream and an S3-compatible store:
//
//	RAWWRITER_IT_NATS_URL=nats://127.0.0.1:24222 \
//	RAWWRITER_IT_S3_ENDPOINT=http://127.0.0.1:28333 \
//	RAWWRITER_IT_S3_ACCESS_KEY=... RAWWRITER_IT_S3_SECRET_KEY=... RAWWRITER_IT_S3_BUCKET=... \
//	go test -tags integration ./internal/natsconsumer/
package natsconsumer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/service"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/storage"
)

func TestEndToEndLandsParquetAndAcks(t *testing.T) {
	natsURL, endpoint := os.Getenv("RAWWRITER_IT_NATS_URL"), os.Getenv("RAWWRITER_IT_S3_ENDPOINT")
	if natsURL == "" || endpoint == "" {
		t.Skip("RAWWRITER_IT_NATS_URL / RAWWRITER_IT_S3_ENDPOINT not set")
	}
	ctx := context.Background()
	id := fmt.Sprint(time.Now().UnixNano())
	streamName, prefix := "IT"+id, "it"+id
	bucket := os.Getenv("RAWWRITER_IT_S3_BUCKET")

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: streamName, Subjects: []string{prefix + ".>"}}); err != nil {
		t.Fatal(err)
	}
	defer js.DeleteStream(ctx, streamName)
	payloads := []string{
		`{"event_id":"e1","app_id":"game","event_name":"start","event_timestamp":"2026-03-16T18:42:15Z","event_params":{"level":3}}`,
		`not json`,
		`{"app_id":"game","user":"bad"}`,
		`{"event_id":"e2","app_id":"game","event_name":"end","event_timestamp":"2026-03-16T18:59:00+00:00"}`,
	}
	for _, p := range payloads {
		if _, err := js.Publish(ctx, prefix+".game", []byte(p)); err != nil {
			t.Fatal(err)
		}
	}

	s, err := settings.FromMap(map[string]string{
		"NATS_URL": natsURL, "NATS_STREAM": streamName, "NATS_SUBJECT_PREFIX": prefix,
		"RAW_STORAGE_BACKEND": "s3", "S3_ENDPOINT_URL": endpoint, "S3_BUCKET": bucket, "S3_PREFIX": prefix,
		"S3_ACCESS_KEY": os.Getenv("RAWWRITER_IT_S3_ACCESS_KEY"), "S3_SECRET_KEY": os.Getenv("RAWWRITER_IT_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := storage.New(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	consumer := New(s)
	svc, err := service.New(s, consumer, uploader)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run() }()
	deadline := time.Now().Add(20 * time.Second)
	for svc.BufferedEvents() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	svc.RequestStop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	cons, _ := js.Consumer(ctx, streamName, "raw-writer")
	info, err := cons.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumAckPending != 0 || info.NumPending != 0 || info.Config.MaxAckPending != 10000 ||
		info.Config.AckWait != 120*time.Second || info.Config.MaxDeliver != 10 || info.Config.FilterSubject != prefix+".>" {
		t.Fatalf("consumer state %+v", info)
	}

	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(s.S3AccessKey, s.S3SecretKey, ""),
	})
	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/")})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Contents) != 1 || !strings.HasPrefix(*list.Contents[0].Key, prefix+"/app_id=game/event_date=2026-03-16/hour=18/part-") {
		t.Fatalf("objects %+v", list.Contents)
	}
	obj, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: list.Contents[0].Key})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(obj.Body)
	reader, err := file.NewParquetReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if reader.NumRows() != 2 {
		t.Fatalf("rows %d", reader.NumRows())
	}
}
