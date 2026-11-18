package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gcsAttemptTimeout bounds one upload attempt, like timeout=60 in the Python client.
const gcsAttemptTimeout = 60 * time.Second

// gcsScopes are the scopes google-cloud-storage requests.
var gcsScopes = []string{
	"https://www.googleapis.com/auth/devstorage.full_control",
	"https://www.googleapis.com/auth/devstorage.read_only",
	"https://www.googleapis.com/auth/devstorage.read_write",
}

// GCSUploader writes objects with the JSON API media upload. Credentials come from
// Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud, metadata
// server); STORAGE_EMULATOR_HOST points it at an unauthenticated emulator instead.
type GCSUploader struct {
	client   *http.Client
	endpoint string
	name     string
	retries  int
}

// NewGCS resolves credentials; it fails when none can be found.
func NewGCS(ctx context.Context, bucket string) (*GCSUploader, error) {
	u := &GCSUploader{name: bucket, retries: DefaultRetries}
	if host := os.Getenv("STORAGE_EMULATOR_HOST"); host != "" {
		if !strings.Contains(host, "://") {
			host = "http://" + host
		}
		u.endpoint, u.client = strings.TrimRight(host, "/"), &http.Client{}
		return u, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, gcsScopes...)
	if err != nil {
		return nil, err
	}
	u.endpoint = "https://storage.googleapis.com"
	u.client = oauth2.NewClient(context.Background(), creds.TokenSource)
	return u, nil
}

// Upload stores data at objectPath, retrying failed attempts.
func (u *GCSUploader) Upload(ctx context.Context, data []byte, objectPath string) error {
	target := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		u.endpoint, url.PathEscape(u.name), strings.ReplaceAll(url.QueryEscape(objectPath), "+", "%20"))
	return withRetries(ctx, "gcs", u.name, objectPath, u.retries, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, gcsAttemptTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := u.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode/100 != 2 {
			return &GCSStatusError{StatusCode: resp.StatusCode}
		}
		return nil
	})
}

// GCSStatusError is a non-2xx response from the JSON API.
type GCSStatusError struct{ StatusCode int }

func (e *GCSStatusError) Error() string {
	return fmt.Sprintf("gcs upload failed with HTTP %d", e.StatusCode)
}

// ErrorClass uses the google.api_core exception names the Python client raised.
func (e *GCSStatusError) ErrorClass() string {
	if name, ok := map[int]string{
		400: "BadRequest", 401: "Unauthorized", 403: "Forbidden", 404: "NotFound", 409: "Conflict",
		412: "PreconditionFailed", 429: "TooManyRequests", 500: "InternalServerError", 502: "BadGateway",
		503: "ServiceUnavailable", 504: "GatewayTimeout",
	}[e.StatusCode]; ok {
		return name
	}
	return fmt.Sprintf("HTTP%d", e.StatusCode)
}
