package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Objects for backend services' storage (V4 §5): plain puts, ranged reads,
// server-side copies, and multipart uploads whose parts clients send
// straight to the store with presigned URLs.

// ErrNotFound is a key that isn't in the store; ErrInvalidRange a range
// outside the object.
var (
	ErrNotFound     = errors.New("no such object")
	ErrInvalidRange = errors.New("the range is outside the object")
)

func notFound(err error) bool {
	var nk *types.NoSuchKey
	var nf *types.NotFound
	var api smithy.APIError
	return errors.As(err, &nk) || errors.As(err, &nf) ||
		(errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchUpload"))
}

// Put stores size bytes from r at key in one request (an upload the edge
// streams, up to its direct limit).
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	// A streamed body can't be hashed ahead for the signature (over plain
	// HTTP the SDK would try to): sign it unsigned, as over TLS.
	out, err := c.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key)),
		Body: r, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)},
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
	if err != nil {
		return "", fmt.Errorf("put %s: %w", key, err)
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

// Object is a read: the body (the range asked for) and what the store says
// about it.
type Object struct {
	Body io.ReadCloser
	// Size is the whole object's; Length the body's.
	Size, Length int64
	// Range is the Content-Range answered for a ranged read.
	Range string
	ETag  string
}

// Get reads key, or the byte range rng ("bytes=0-99", empty for all).
func (c *Client) Get(ctx context.Context, key, rng string) (Object, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key))}
	if rng != "" {
		in.Range = aws.String(rng)
	}
	out, err := c.s3.GetObject(ctx, in)
	if err != nil {
		if notFound(err) {
			return Object{}, ErrNotFound
		}
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "InvalidRange" {
			return Object{}, ErrInvalidRange
		}
		return Object{}, fmt.Errorf("get %s: %w", key, err)
	}
	o := Object{Body: out.Body, Length: aws.ToInt64(out.ContentLength), Range: aws.ToString(out.ContentRange),
		ETag: strings.Trim(aws.ToString(out.ETag), `"`)}
	o.Size = o.Length
	if i := strings.LastIndexByte(o.Range, '/'); i >= 0 {
		var n int64
		if _, err := fmt.Sscan(o.Range[i+1:], &n); err == nil {
			o.Size = n
		}
	}
	return o, nil
}

// Head is an object's size and ETag; ErrNotFound when it isn't there.
func (c *Client) Head(ctx context.Context, key string) (int64, string, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key))})
	if err != nil {
		if notFound(err) {
			return 0, "", ErrNotFound
		}
		return 0, "", fmt.Errorf("head %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), strings.Trim(aws.ToString(out.ETag), `"`), nil
}

// Copy copies src to dst inside the target's bucket (up to 5 GB, S3's limit
// for a single copy).
func (c *Client) Copy(ctx context.Context, src, dst string) (string, error) {
	source := c.t.Bucket + "/" + c.t.Key(src)
	out, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(dst)),
		CopySource: aws.String(url.PathEscape(source))})
	if err != nil {
		if notFound(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("copy %s: %w", src, err)
	}
	etag := ""
	if out.CopyObjectResult != nil {
		etag = strings.Trim(aws.ToString(out.CopyObjectResult.ETag), `"`)
	}
	return etag, nil
}

// Entry is a listed object.
type Entry struct {
	Key      string // relative to the target's prefix
	Size     int64
	Modified time.Time
}

// List calls fn for every object under rel, a page at a time.
func (c *Client) List(ctx context.Context, rel string, fn func([]Entry) error) error {
	prefix := c.t.Key(rel)
	base := c.t.Key("")
	var token *string
	for {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.t.Bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		page := make([]Entry, 0, len(out.Contents))
		for _, o := range out.Contents {
			page = append(page, Entry{Key: strings.TrimPrefix(aws.ToString(o.Key), base), Size: aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified)})
		}
		if err := fn(page); err != nil {
			return err
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// StartMultipart begins a multipart upload to key and returns its upload id.
func (c *Client) StartMultipart(ctx context.Context, key, contentType string) (string, error) {
	out, err := c.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(c.t.Bucket),
		Key: aws.String(c.t.Key(key)), ContentType: aws.String(contentType)})
	if err != nil {
		return "", fmt.Errorf("start upload %s: %w", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

// PresignPart is a URL a client PUTs part n (1-based) of the upload to,
// valid for ttl. The part's size is signed in (Content-Length is a signed
// header), so the store takes exactly size bytes there and no more.
func (c *Client) PresignPart(ctx context.Context, key, uploadID string, n int32, size int64, ttl time.Duration) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("presign part %d: a part has a size", n)
	}
	ps := s3.NewPresignClient(c.s3)
	req, err := ps.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key)),
		UploadId: aws.String(uploadID), PartNumber: aws.Int32(n), ContentLength: aws.Int64(size)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign part %d: %w", n, err)
	}
	return req.URL, nil
}

// Part is an uploaded part.
type Part struct {
	Number int32
	Size   int64
	ETag   string
}

// Parts lists the parts uploaded so far, in order; ErrNotFound when the
// upload is gone (completed, aborted or expired).
func (c *Client) Parts(ctx context.Context, key, uploadID string) ([]Part, error) {
	var out []Part
	var marker *string
	for {
		res, err := c.s3.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(c.t.Bucket), Key: aws.String(c.t.Key(key)),
			UploadId: aws.String(uploadID), PartNumberMarker: marker})
		if err != nil {
			if notFound(err) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("list parts: %w", err)
		}
		for _, p := range res.Parts {
			out = append(out, Part{Number: aws.ToInt32(p.PartNumber), Size: aws.ToInt64(p.Size), ETag: aws.ToString(p.ETag)})
		}
		if !aws.ToBool(res.IsTruncated) {
			break
		}
		marker = res.NextPartNumberMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// CompleteMultipart joins parts into the object.
func (c *Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (string, error) {
	done := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		done = append(done, types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag)})
	}
	out, err := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(c.t.Bucket),
		Key: aws.String(c.t.Key(key)), UploadId: aws.String(uploadID), MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	if err != nil {
		if notFound(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("complete upload: %w", err)
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

// AbortMultipart abandons an upload and its parts; one already gone is not
// an error.
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(c.t.Bucket),
		Key: aws.String(c.t.Key(key)), UploadId: aws.String(uploadID)})
	if err != nil && !notFound(err) {
		return fmt.Errorf("abort upload: %w", err)
	}
	return nil
}

// Upload is a multipart upload in progress.
type Upload struct {
	Key, ID   string
	Initiated time.Time
}

// Multiparts lists the uploads in progress under rel.
func (c *Client) Multiparts(ctx context.Context, rel string) ([]Upload, error) {
	prefix, base := c.t.Key(rel), c.t.Key("")
	var out []Upload
	var keyMarker, idMarker *string
	for {
		res, err := c.s3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(c.t.Bucket), Prefix: aws.String(prefix),
			KeyMarker: keyMarker, UploadIdMarker: idMarker})
		if err != nil && notFound(err) {
			return out, nil // some stores answer so when there are none
		}
		if err != nil {
			return nil, fmt.Errorf("list uploads: %w", err)
		}
		for _, u := range res.Uploads {
			out = append(out, Upload{Key: strings.TrimPrefix(aws.ToString(u.Key), base), ID: aws.ToString(u.UploadId),
				Initiated: aws.ToTime(u.Initiated)})
		}
		if !aws.ToBool(res.IsTruncated) {
			return out, nil
		}
		keyMarker, idMarker = res.NextKeyMarker, res.NextUploadIdMarker
	}
}
