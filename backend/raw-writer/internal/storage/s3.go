package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config holds the S3-compatible connection settings. The scheme of EndpointURL
// decides between HTTP and HTTPS, as it did with boto3 (S3_USE_SSL has no effect once an
// endpoint URL is given).
type S3Config struct {
	EndpointURL    string
	Bucket         string
	AccessKey      string
	SecretKey      string
	Region         string
	ForcePathStyle bool
	VerifySSL      bool
}

// PutObjectAPI is the part of the S3 client the uploader uses.
type PutObjectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3Uploader writes objects with PutObject.
type S3Uploader struct {
	client  PutObjectAPI
	bucket  string
	retries int
}

// NewS3 builds a client for a custom endpoint with static credentials.
func NewS3(cfg S3Config) (*S3Uploader, error) {
	endpoint, err := url.Parse(cfg.EndpointURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("Invalid endpoint: %s", cfg.EndpointURL)
	}
	httpClient := awshttp.NewBuildableClient()
	if !cfg.VerifySSL {
		httpClient = httpClient.WithTransportOptions(func(t *http.Transport) {
			if t.TLSClientConfig == nil {
				t.TLSClientConfig = &tls.Config{}
			}
			t.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // S3_VERIFY_SSL=0 asks for it
		})
	}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.EndpointURL),
		UsePathStyle: cfg.ForcePathStyle,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		HTTPClient:   httpClient,
	})
	return NewS3WithClient(client, cfg.Bucket), nil
}

// NewS3WithClient wraps an existing client.
func NewS3WithClient(client PutObjectAPI, bucket string) *S3Uploader {
	return &S3Uploader{client: client, bucket: bucket, retries: DefaultRetries}
}

// Upload stores data at objectPath, retrying failed attempts.
func (u *S3Uploader) Upload(ctx context.Context, data []byte, objectPath string) error {
	return withRetries(ctx, "s3", u.bucket, objectPath, u.retries, func(ctx context.Context) error {
		_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(u.bucket),
			Key:    aws.String(objectPath),
			Body:   bytes.NewReader(data),
		})
		return err
	})
}
