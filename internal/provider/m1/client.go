package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

const (
	maxObjectBytes        = 512 << 20
	maxJSONBytes          = 16 << 20
	maxErrorResponseBytes = 64 << 10
)

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
	if !validEndpointURL(endpoint) {
		return nil, fmt.Errorf("m1 endpoint must be an HTTPS URL without credentials, query, or fragment")
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
		HTTPClient:  providerHTTPClient(18 * time.Second),
	}
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &Client{bucket: bucket, s3: api, presign: s3.NewPresignClient(api)}, nil
}

func validEndpointURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	// Local HTTP endpoints are useful for isolated development and tests. Never
	// send signing credentials over plaintext to a non-loopback host.
	return u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())
}

func providerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: boundedErrorResponseTransport{base: http.DefaultTransport},
		// Object-store requests are signed with the configured credentials. A
		// redirect target must not receive that signed request or its body.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// The AWS SDK buffers S3 error responses while decoding them. Bound those
// bodies before they reach the SDK, while leaving successful object and list
// responses untouched.
type boundedErrorResponseTransport struct {
	base http.RoundTripper
}

func (t boundedErrorResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil || response == nil || response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices || response.Body == nil {
		return response, err
	}
	response.Body = &limitedReadCloser{Reader: io.LimitReader(response.Body, maxErrorResponseBytes), closer: response.Body}
	return response, nil
}

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *limitedReadCloser) Close() error { return r.closer.Close() }

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

func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.bucket, Key: &key})
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
	b, _, err := c.getBytesVersion(ctx, key, maxJSONBytes)
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
	return c.getBytesVersion(ctx, key, maxObjectBytes)
}

// GetBytesLimited applies the caller's resource budget before retaining an
// object. Oversized objects never return a truncated successful result.
func (c *Client) GetBytesLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if limit < 1 || limit > maxObjectBytes {
		return nil, fmt.Errorf("object limit must be between 1 and %d bytes", maxObjectBytes)
	}
	b, _, err := c.getBytesVersion(ctx, key, limit)
	return b, err
}

func (c *Client) getBytesVersion(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		return nil, "", err
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > limit {
		return nil, "", fmt.Errorf("object exceeds %d bytes", limit)
	}
	b, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > limit {
		return nil, "", fmt.Errorf("object exceeds %d bytes", limit)
	}
	return b, aws.ToString(out.ETag), nil
}

func (c *Client) GetJSONVersion(ctx context.Context, key string, target any) (bool, string, error) {
	b, etag, err := c.getBytesVersion(ctx, key, maxJSONBytes)
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

// StoredBytes returns a complete, paginated inventory of current objects.
// A partial or failed listing is never used for billing.
func (c *Client) StoredBytes(ctx context.Context, prefix string) (int64, error) {
	if prefix == "" || prefix[len(prefix)-1] != '/' {
		return 0, fmt.Errorf("metering requires a delimited namespace")
	}
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{Bucket: &c.bucket, Prefix: &prefix})
	var total int64
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, object := range page.Contents {
			size := aws.ToInt64(object.Size)
			if size < 0 || total > 1<<60-size {
				return 0, fmt.Errorf("invalid storage inventory size")
			}
			total += size
		}
	}
	return total, nil
}
