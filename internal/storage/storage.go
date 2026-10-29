// Package storage talks to S3-compatible object storage (Cloudflare R2,
// Backblaze B2, MinIO, AWS) for backups (spec §3.2).
package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Target is a bucket location and its credentials. It travels to agents
// (over mTLS) for each dump or restore, so they hold no standing secrets.
type Target struct {
	Endpoint  string `json:"endpoint"` // e.g. https://<account>.r2.cloudflarestorage.com
	Region    string `json:"region"`   // "auto" for R2
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"` // optional key prefix, e.g. "xenibase/"
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	// PathStyle addresses the bucket in the path (MinIO, most self-hosted).
	PathStyle bool `json:"path_style"`
}

// Validate checks a target's fields.
func (t Target) Validate() error {
	u, err := url.Parse(t.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("endpoint must be an http(s) URL")
	}
	if t.Bucket == "" || strings.ContainsAny(t.Bucket, "/ ") {
		return errors.New("bucket is required and must not contain slashes or spaces")
	}
	if t.AccessKey == "" || t.SecretKey == "" {
		return errors.New("access key and secret key are required")
	}
	if strings.HasPrefix(t.Prefix, "/") {
		return errors.New("prefix must not start with a slash")
	}
	return nil
}

// Key joins the target's prefix with a relative object key.
func (t Target) Key(rel string) string {
	p := t.Prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p + strings.TrimPrefix(rel, "/")
}

// Location identifies where the target's objects are: the endpoint (scheme
// and host in lower case, without a default port or trailing slash), the
// bucket and the prefix as Key applies it. Two targets with one location
// read and write the same objects, whatever their names or credentials.
func (t Target) Location() string {
	ep := strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			u.Host = u.Hostname()
		}
		u.Path = strings.TrimRight(u.Path, "/")
		ep = u.String()
	}
	return ep + " " + strings.TrimSpace(t.Bucket) + "/" + t.Key("")
}

// Redacted returns the target without credentials, for logs and the API.
func (t Target) Redacted() Target {
	t.SecretKey = ""
	if len(t.AccessKey) > 4 {
		t.AccessKey = t.AccessKey[:4] + "…"
	}
	return t
}

// Client is an S3 client for one target.
type Client struct {
	t  Target
	s3 *s3.Client
}

// New returns a client for t.
func New(t Target) (*Client, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	region := t.Region
	if region == "" {
		region = "auto"
	}
	c := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(t.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(t.AccessKey, t.SecretKey, ""),
		UsePathStyle: t.PathStyle,
		HTTPClient:   &http.Client{Timeout: 0, Transport: transport()}, // streaming bodies; contexts bound requests
		// R2 and many S3-compatible stores reject the newer default
		// checksum headers on streamed uploads.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Client{t: t, s3: c}, nil
}

// Timeouts for reaching a store. They bound the connection and the wait for
// a reply, not the body: a stalled or unreachable endpoint fails in seconds
// instead of holding an operation (and its place in the organisation's
// operations-in-flight quota) until the operation's own 30-minute limit.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 2 * time.Minute
)

func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// Target returns the client's target.
func (c *Client) Target() Target { return c.t }

// Upload streams r to key (relative to the prefix) with multipart upload.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader) error {
	//nolint:staticcheck // transfermanager is still a preview module; manager works and is supported.
	up := manager.NewUploader(c.s3, func(u *manager.Uploader) {
		u.PartSize = 16 << 20
		u.Concurrency = 2
	})
	_, err := up.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // see above
		Bucket: aws.String(c.t.Bucket),
		Key:    aws.String(c.t.Key(key)),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

// Download opens key for reading. The caller closes it.
func (c *Client) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key))})
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes key; a missing key is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key))})
	var nf *types.NoSuchKey
	if err != nil && !errors.As(err, &nf) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// DeletePrefix deletes every object under rel (relative to the target's
// prefix) and returns how many it removed.
func (c *Client) DeletePrefix(ctx context.Context, rel string) (int, error) {
	prefix := strings.TrimSuffix(c.t.Key(rel), "/") + "/"
	var n int
	var token *string
	for {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.t.Bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return n, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range out.Contents {
			if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.t.Bucket), Key: o.Key}); err != nil {
				return n, fmt.Errorf("delete %s: %w", aws.ToString(o.Key), err)
			}
			n++
		}
		if !aws.ToBool(out.IsTruncated) {
			return n, nil
		}
		token = out.NextContinuationToken
	}
}

// Size returns an object's size, or an error if it does not exist.
func (c *Client) Size(ctx context.Context, key string) (int64, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key))})
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// TestStep is one step of a live test.
type TestStep struct {
	Step string
	OK   bool
	Err  string
	Took time.Duration
}

// LiveTest writes, reads back, lists, and deletes a small object under the
// target's prefix, as saving a target requires (spec §8.2, V2 §6).
func (c *Client) LiveTest(ctx context.Context) ([]TestStep, bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := "xenibase-write-test/" + hex.EncodeToString(b)
	payload := []byte("xenibase storage test " + hex.EncodeToString(b))

	var steps []TestStep
	run := func(name string, f func() error) bool {
		start := time.Now()
		err := f()
		s := TestStep{Step: name, OK: err == nil, Took: time.Since(start)}
		if err != nil {
			s.Err = err.Error()
		}
		steps = append(steps, s)
		return err == nil
	}
	ok := run("write", func() error {
		_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key)), Body: bytes.NewReader(payload),
		})
		return err
	}) && run("read", func() error {
		rc, err := c.Download(ctx, key)
		if err != nil {
			return err
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, payload) {
			return errors.New("read back different bytes than were written")
		}
		return nil
	}) && run("list", func() error {
		want := c.t.Key(key)
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.t.Bucket), Prefix: aws.String(want)})
		if err != nil {
			return err
		}
		for _, o := range out.Contents {
			if aws.ToString(o.Key) == want {
				return nil
			}
		}
		return errors.New("the object just written is not in the listing")
	}) && run("delete", func() error { return c.Delete(ctx, key) })
	return steps, ok
}
