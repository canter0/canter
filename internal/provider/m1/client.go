package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type Client struct {
	bucket  string
	s3      *s3.Client
	presign *s3.PresignClient
}

type ProbeResult struct {
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
}

func NewFromEnv() (*Client, error) {
	endpoint := os.Getenv("CANTER_M1_ENDPOINT")
	bucket := os.Getenv("CANTER_M1_BUCKET")
	access := os.Getenv("CANTER_M1_ACCESS_KEY")
	secret := os.Getenv("CANTER_M1_SECRET_KEY")
	region := os.Getenv("CANTER_M1_REGION")
	if region == "" {
		region = "auto"
	}
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		return nil, fmt.Errorf("m1 credentials are incomplete")
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
		HTTPClient:  &http.Client{Timeout: 18 * time.Second},
	}
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &Client{bucket: bucket, s3: api, presign: s3.NewPresignClient(api)}, nil
}

func (c *Client) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.bucket})
	r := ProbeResult{OK: err == nil, Latency: time.Since(start)}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

func (c *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket, Key: &key, Body: bytes.NewReader(data), ContentType: &contentType,
	})
	return err
}

func (c *Client) PutJSON(ctx context.Context, key string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return c.Put(ctx, key, b, "application/json")
}

func (c *Client) PutJSONIfAbsent(ctx context.Context, key string, value any) (string, bool, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", false, err
	}
	contentType := "application/json"
	condition := "*"
	out, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket, Key: &key, Body: bytes.NewReader(b), ContentType: &contentType, IfNoneMatch: &condition,
	})
	if isPrecondition(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return aws.ToString(out.ETag), true, nil
}

func (c *Client) PutJSONIfMatch(ctx context.Context, key, etag string, value any) (string, bool, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", false, err
	}
	contentType := "application/json"
	out, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket, Key: &key, Body: bytes.NewReader(b), ContentType: &contentType, IfMatch: &etag,
	})
	if isPrecondition(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return aws.ToString(out.ETag), true, nil
}

func (c *Client) Get(ctx context.Context, key string, target any) error {
	b, err := c.GetBytes(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	b, _, err := c.GetBytesVersion(ctx, key)
	return b, err
}

func (c *Client) GetBytesVersion(ctx context.Context, key string) ([]byte, string, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		return nil, "", err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, 512<<20))
	if err != nil {
		return nil, "", err
	}
	return b, aws.ToString(out.ETag), nil
}

func (c *Client) GetJSONVersion(ctx context.Context, key string, target any) (bool, string, error) {
	b, etag, err := c.GetBytesVersion(ctx, key)
	if err == nil {
		return true, etag, json.Unmarshal(b, target)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
		return false, "", nil
	}
	return false, "", err
}

func (c *Client) GetOptional(ctx context.Context, key string, target any) (bool, error) {
	err := c.Get(ctx, key, target)
	if err == nil {
		return true, nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
		return false, nil
	}
	return false, err
}

func (c *Client) Exists(ctx context.Context, key string) bool {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	return err == nil
}

func (c *Client) PresignPut(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	out, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket, Key: &key, ContentType: aws.String("application/json"),
	}, func(o *s3.PresignOptions) { o.Expires = lifetime })
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) PresignGet(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	out, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key}, func(o *s3.PresignOptions) {
		o.Expires = lifetime
	})
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func isPrecondition(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "PreconditionFailed" || apiErr.ErrorCode() == "ConditionalRequestConflict")
}
