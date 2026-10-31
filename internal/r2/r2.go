// Package r2 reads and manages the two Cloudflare R2 buckets of the ingest
// contract (§2.2) through the S3 API.
//
// The raw bucket is exposed read-only on purpose: its type has no method
// that could issue a write or delete, and the deployment gives it a
// read-only token, so "raw is immutable" holds by construction rather than
// by discipline. The pending bucket has its own credentials.
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// MaxObjectSize bounds how much of one object is read into memory. Inbound
// mail is capped at 25 MiB by Email Routing; anything larger is a bug.
const MaxObjectSize = 32 << 20

// ErrNotFound is returned (wrapped) when a key does not exist.
var ErrNotFound = errors.New("r2: object not found")

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

// RawBucket is read-only access to raw/YYYY/MM/<id>.eml objects.
type RawBucket struct{ b bucket }

// NewRawBucket returns a client for the raw bucket. Give it a read-only token.
func NewRawBucket(c Config) *RawBucket {
	return &RawBucket{bucket{newClient(c), c.Bucket}}
}

// Get reads a raw object and its metadata.
func (r *RawBucket) Get(ctx context.Context, key string) (Object, error) { return r.b.get(ctx, key) }

// List yields every key under prefix.
func (r *RawBucket) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return r.b.list(ctx, prefix)
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
