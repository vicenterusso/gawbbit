// Package storage uploads finished Parquet files to GCS or an S3-compatible store.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/errclass"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
)

// Uploader stores one object.
type Uploader interface {
	Upload(ctx context.Context, data []byte, objectPath string) error
}

// DefaultRetries is how many attempts an upload gets before the flush fails.
const DefaultRetries = 3

// New builds the uploader for the configured backend.
func New(ctx context.Context, s *settings.Settings) (Uploader, error) {
	switch s.RawStorageBackend {
	case "gcs":
		return NewGCS(ctx, s.GCSRawBucket)
	case "s3":
		return NewS3(S3Config{
			EndpointURL:    s.S3EndpointURL,
			Bucket:         s.S3Bucket,
			AccessKey:      s.S3AccessKey,
			SecretKey:      s.S3SecretKey,
			Region:         s.S3Region,
			ForcePathStyle: s.S3ForcePathStyle,
			VerifySSL:      s.S3VerifySSL,
		})
	}
	return nil, fmt.Errorf("Unsupported RAW_STORAGE_BACKEND: %s", s.RawStorageBackend)
}

// sleep is replaced in tests.
var sleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// withRetries runs attempt up to retries times, waiting 2s, 4s, ... between attempts and
// logging each failure without the error text.
func withRetries(ctx context.Context, backend, bucket, objectPath string, retries int, attempt func(context.Context) error) error {
	for n := 1; ; n++ {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		logx.Std.Warningf("upload_retry backend=%s bucket=%s object_path=%s attempt=%d/%d error_class=%s",
			backend, bucket, objectPath, n, retries, errclass.Of(err))
		if n >= retries {
			return err
		}
		sleep(ctx, time.Duration(1<<n)*time.Second)
	}
}
