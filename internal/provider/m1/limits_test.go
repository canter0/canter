package m1

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type objectRoundTripper func(*http.Request) (*http.Response, error)

func (f objectRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackedObjectBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *trackedObjectBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedObjectBody) Close() error {
	b.closed = true
	return nil
}

func objectTestClient(body *trackedObjectBody, advertisedLength int64) *Client {
	transport := objectRoundTripper(func(request *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {`"fixture"`}}
		if advertisedLength >= 0 {
			header.Set("Content-Length", strconv.FormatInt(advertisedLength, 10))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body, ContentLength: advertisedLength, Request: request}, nil
	})
	api := s3.NewFromConfig(aws.Config{
		Region: "auto", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""),
		HTTPClient: &http.Client{Transport: boundedErrorResponseTransport{base: transport}},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("https://objects.example")
		options.UsePathStyle = true
	})
	return &Client{bucket: "fixture", s3: api}
}

func TestObjectLimitsRejectAdvertisedOversizeWithoutReadingBody(t *testing.T) {
	for _, operation := range []string{"bytes", "json", "versioned-json", "optional-json"} {
		t.Run(operation, func(t *testing.T) {
			limit := int64(maxJSONBytes)
			if operation == "bytes" {
				limit = maxObjectBytes
			}
			body := &trackedObjectBody{Reader: strings.NewReader(`{"safe":true}`)}
			client := objectTestClient(body, limit+1)
			var target map[string]any
			var err error
			switch operation {
			case "bytes":
				var data []byte
				data, _, err = client.GetBytesVersion(context.Background(), "fixture")
				if data != nil {
					t.Fatal("oversized object returned partial bytes")
				}
			case "json":
				err = client.Get(context.Background(), "fixture", &target)
			case "versioned-json":
				_, _, err = client.GetJSONVersion(context.Background(), "fixture", &target)
			case "optional-json":
				_, err = client.GetOptional(context.Background(), "fixture", &target)
			}
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("oversized object was accepted: %v", err)
			}
			if body.read != 0 || !body.closed || target != nil {
				t.Fatalf("oversized object read=%d closed=%v target=%v", body.read, body.closed, target)
			}
		})
	}
}

func TestLimitedObjectRejectsUnknownLengthOverflowAndAcceptsExactLimit(t *testing.T) {
	for _, length := range []int{32, 33} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			body := &trackedObjectBody{Reader: strings.NewReader(strings.Repeat("x", length))}
			client := objectTestClient(body, -1)
			data, err := client.GetBytesLimited(context.Background(), "fixture", 32)
			if length == 32 {
				if err != nil || len(data) != 32 {
					t.Fatalf("exact limit was rejected: len=%d err=%v", len(data), err)
				}
			} else if err == nil || data != nil {
				t.Fatalf("overflow returned success or partial bytes: len=%d err=%v", len(data), err)
			}
			if !body.closed || body.read > 33 {
				t.Fatalf("read=%d closed=%v", body.read, body.closed)
			}
		})
	}
}

func TestObjectJSONStillRejectsTrailingData(t *testing.T) {
	for _, document := range []string{`{"safe":true}`, `{"safe":true} {"unsafe":true}`} {
		body := &trackedObjectBody{Reader: strings.NewReader(document)}
		client := objectTestClient(body, int64(len(document)))
		var target map[string]any
		err := client.Get(context.Background(), "fixture", &target)
		if (err == nil) != (document == `{"safe":true}`) || !body.closed {
			t.Fatalf("document=%q err=%v closed=%v", document, err, body.closed)
		}
	}
}

func TestS3ErrorBodyIsBoundedBeforeSDKDeserialization(t *testing.T) {
	body := &trackedObjectBody{Reader: strings.NewReader(strings.Repeat("x", maxErrorResponseBytes*8))}
	base := objectRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": {"application/xml"}},
			Body:       body,
			Request:    request,
		}, nil
	})
	api := s3.NewFromConfig(aws.Config{
		Region: "auto", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""),
		HTTPClient: &http.Client{Transport: boundedErrorResponseTransport{base: base}},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("https://objects.example")
		options.UsePathStyle = true
	})
	client := &Client{bucket: "fixture", s3: api}
	_, err := client.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("fixture"), Key: aws.String("fixture")})
	if err == nil {
		t.Fatal("expected S3 error")
	}
	if body.read != maxErrorResponseBytes || !body.closed {
		t.Fatalf("error body read=%d closed=%v; want read at most %d and closed", body.read, body.closed, maxErrorResponseBytes)
	}
}

func TestBoundedErrorTransportLeavesSuccessfulBodiesUntouched(t *testing.T) {
	body := &trackedObjectBody{Reader: strings.NewReader(strings.Repeat("x", maxErrorResponseBytes+1))}
	base := objectRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Request: request}, nil
	})
	response, err := (boundedErrorResponseTransport{base: base}).RoundTrip(&http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || len(data) != maxErrorResponseBytes+1 {
		t.Fatalf("successful body was altered: read=%d err=%v", len(data), err)
	}
}
