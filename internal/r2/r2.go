// Package r2 reads and manages the two Cloudflare R2 buckets of the ingest
// contract (§2.2) through the S3 API.
//
// The raw bucket holds two kinds of immutable MIME: raw/ (inbound, written
// by the Worker) and sent/ (every byte string yuuka composed or normalized
// for sending; whether it went out is outbox state, §4.6). Go's token for
// it can write, because of sent/, so immutability is kept here rather than
// by the token (§2.2): RawBucket has no delete, and its one write, PutSent,
// refuses any key that is not a sent copy key before a request is made and
// is create-only (If-None-Match: *), so even a bug that names an existing
// key cannot replace it. The pending bucket has its own credentials.
package r2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// MaxObjectSize bounds how much of one object is read into memory. Inbound
// mail is capped at 25 MiB by Email Routing; anything larger is a bug.
const MaxObjectSize = 32 << 20

// ErrNotFound is returned (wrapped) when a key does not exist.
var ErrNotFound = errors.New("r2: object not found")

// ErrExists is returned (wrapped) by a create-only write when the key is
// already taken. A retried write whose first attempt did land ends here
// too, so the caller decides by comparing the stored bytes with its own.
var ErrExists = errors.New("r2: object already exists")

// SentKey is the raw bucket key of a sent copy: sent/YYYY/MM/<id>.eml, with
// year and month taken from submittedAt in UTC and id a ULID. It is not
// part of the Worker contract: the Worker never reads or writes sent/.
func SentKey(submittedAt time.Time, id string) string {
	t := submittedAt.UTC()
	return fmt.Sprintf("sent/%04d/%02d/%s.eml", t.Year(), int(t.Month()), id)
}

// CheckSentKey accepts exactly the keys SentKey produces for a valid ULID.
// It is the only thing between Go's writable raw bucket token and raw/, so
// it checks the whole shape rather than the prefix.
func CheckSentKey(key string) error {
	parts := strings.Split(key, "/")
	ok := len(parts) == 4 && parts[0] == "sent" &&
		digitsInRange(parts[1], 4, 1, 9999) && digitsInRange(parts[2], 2, 1, 12)
	if ok {
		id, eml := strings.CutSuffix(parts[3], ".eml")
		ok = eml && ulid.Valid(id)
	}
	if !ok {
		return fmt.Errorf("r2: %q is not a sent copy key", key)
	}
	return nil
}

// digitsInRange reports whether s is exactly n decimal digits in [lo, hi].
func digitsInRange(s string, n, lo, hi int) bool {
	if len(s) != n || strings.Trim(s, "0123456789") != "" {
		return false
	}
	v, err := strconv.Atoi(s)
	return err == nil && v >= lo && v <= hi
}

// Object is a whole object read into memory.
type Object struct {
	Body []byte
	// Metadata holds the custom metadata with lowercased keys.
	Metadata map[string]string
}

// Config addresses one bucket with its own credentials.
type Config struct {
	Endpoint        string // https://<account id>.r2.cloudflarestorage.com
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	// HTTPClient is optional; tests point it at a local server.
	HTTPClient s3.HTTPClient
}

// newClient builds the S3 client without the SDK's config package, which
// would pull in shared-config files, IMDS and SSO that R2 never uses.
func newClient(c Config) *s3.Client {
	return s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(c.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""),
		// Since 2025 the SDK adds checksums by default that R2 does not
		// fully support (§5).
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		// Path style keeps the bucket out of the hostname, so any endpoint
		// works without wildcard DNS or certificates.
		UsePathStyle: true,
		HTTPClient:   c.HTTPClient,
	})
}

type bucket struct {
	client *s3.Client
	name   string
}

func (b bucket) get(ctx context.Context, key string) (Object, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.name, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return Object{}, fmt.Errorf("get %s/%s: %w", b.name, key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("get %s/%s: %w", b.name, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, MaxObjectSize+1))
	if err != nil {
		return Object{}, fmt.Errorf("read %s/%s: %w", b.name, key, err)
	}
	if len(body) > MaxObjectSize {
		return Object{}, fmt.Errorf("read %s/%s: larger than %d bytes", b.name, key, MaxObjectSize)
	}
	meta := make(map[string]string, len(out.Metadata))
	for k, v := range out.Metadata {
		meta[strings.ToLower(k)] = v
	}
	return Object{Body: body, Metadata: meta}, nil
}

func (b bucket) list(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		p := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{Bucket: &b.name, Prefix: &prefix})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield("", fmt.Errorf("list %s/%s: %w", b.name, prefix, err))
				return
			}
			for _, o := range page.Contents {
				if !yield(aws.ToString(o.Key), nil) {
					return
				}
			}
		}
	}
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound")
}

// isPreconditionFailed recognises the 412 a create-only PUT gets when the
// key exists. The status decides; the error code is a fallback in case a
// wrapper hides the HTTP response.
func isPreconditionFailed(err error) bool {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "PreconditionFailed"
}

// RawBucket reads raw/ and sent/ objects and creates sent/ objects. It has
// no way to overwrite or delete anything.
type RawBucket struct{ b bucket }

// NewRawBucket returns a client for the raw bucket. Its token needs object
// read and write; the write is for sent copies only (§2.2).
func NewRawBucket(c Config) *RawBucket {
	return &RawBucket{bucket{newClient(c), c.Bucket}}
}

// Get reads a raw object and its metadata.
func (r *RawBucket) Get(ctx context.Context, key string) (Object, error) { return r.b.get(ctx, key) }

// List yields every key under prefix.
func (r *RawBucket) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return r.b.list(ctx, prefix)
}

// PutSent stores body under key, which must pass CheckSentKey, only if
// nothing is stored there yet. R2 evaluates If-None-Match: * atomically with
// the write, so of concurrent writers at most one succeeds; the others, like
// any write to an existing key, get an error wrapping ErrExists and leave
// the stored object as it was.
func (r *RawBucket) PutSent(ctx context.Context, key string, body []byte) error {
	if err := CheckSentKey(key); err != nil {
		return err
	}
	if len(body) > MaxObjectSize {
		// Get could not read it back.
		return fmt.Errorf("put %s/%s: larger than %d bytes", r.b.name, key, MaxObjectSize)
	}
	_, err := r.b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &r.b.name,
		Key:           &key,
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			return fmt.Errorf("put %s/%s: %w", r.b.name, key, ErrExists)
		}
		return fmt.Errorf("put %s/%s: %w", r.b.name, key, err)
	}
	return nil
}

// PendingBucket reads, lists and deletes pending/<id> markers.
type PendingBucket struct{ b bucket }

// NewPendingBucket returns a client for the pending bucket.
func NewPendingBucket(c Config) *PendingBucket {
	return &PendingBucket{bucket{newClient(c), c.Bucket}}
}

// Get reads a marker object.
func (p *PendingBucket) Get(ctx context.Context, key string) (Object, error) {
	return p.b.get(ctx, key)
}

// List yields every key under prefix.
func (p *PendingBucket) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return p.b.list(ctx, prefix)
}

// Delete removes a marker. Deleting a missing key succeeds, as in S3.
func (p *PendingBucket) Delete(ctx context.Context, key string) error {
	if _, err := p.b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &p.b.name, Key: &key}); err != nil {
		return fmt.Errorf("delete %s/%s: %w", p.b.name, key, err)
	}
	return nil
}
