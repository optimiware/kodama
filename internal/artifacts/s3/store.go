// Package s3 is the S3-compatible object-store adapter of the artifact module.
//
// It owns the bucket and key prefix, signs short-lived transfer instructions
// and performs the byte operations the module needs: reading uploaded objects,
// sealing verified bytes and deleting objects. The AWS SDK client is injected;
// no SDK type crosses the module's ObjectStore port. Development uses the
// LocalStack endpoint with path-style SigV4; production requires https
// endpoints.
package s3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/optimiware/kodama/internal/artifacts"
)

// Config configures a Store.
type Config struct {
	// Client performs reads, writes and deletes. Required.
	Client *awss3.Client
	// TransferClient signs instructions. Optional: when nil, Client signs. Use a
	// separate client when the endpoint reachable by producers (Pods, readers)
	// differs from the server's own endpoint.
	TransferClient *awss3.Client
	Bucket         string
	// Prefix scopes every key. The adapter never reads, writes or deletes
	// outside it. It is normalized to end with "/" unless empty.
	Prefix string
	// MaxTransfer caps the lifetime of any signed instruction (SigV4 allows at
	// most seven days). Zero selects one hour.
	MaxTransfer time.Duration
	// AllowInsecure permits http transfer URLs (development fixtures only).
	// Production must leave it false: signed URLs travel over https.
	AllowInsecure bool
	// Clock defaults to the wall clock.
	Clock func() time.Time
}

// Store implements artifacts.ObjectStore.
type Store struct {
	cfg     Config
	presign *awss3.PresignClient
	now     func() time.Time
}

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.Client == nil {
		return nil, errors.New("artifacts/s3: a client is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("artifacts/s3: a bucket is required")
	}
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	if strings.HasPrefix(cfg.Prefix, "/") || strings.Contains(cfg.Prefix, "..") {
		return nil, errors.New("artifacts/s3: the prefix must be relative and free of dot segments")
	}
	if cfg.MaxTransfer <= 0 {
		cfg.MaxTransfer = time.Hour
	}
	if cfg.MaxTransfer > 7*24*time.Hour {
		return nil, errors.New("artifacts/s3: MaxTransfer exceeds the SigV4 limit")
	}
	signer := cfg.TransferClient
	if signer == nil {
		signer = cfg.Client
	}
	s := &Store{cfg: cfg, presign: awss3.NewPresignClient(signer), now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

var _ artifacts.ObjectStore = (*Store)(nil)

// Prefix returns the normalized key prefix the adapter owns.
func (s *Store) Prefix() string { return s.cfg.Prefix }

// objectKey maps a logical key into the owned prefix. Only module-shaped keys
// are accepted, so a caller can never address anything else.
func (s *Store) objectKey(key string) (string, error) {
	if !strings.HasPrefix(key, "artifacts/") || strings.Contains(key, "..") || strings.Contains(key, "//") || strings.HasSuffix(key, "/") {
		return "", fmt.Errorf("%w: object key is outside the artifact namespace", artifacts.ErrInvalid)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '_', r == '-':
		default:
			return "", fmt.Errorf("%w: object key has an unexpected character", artifacts.ErrInvalid)
		}
	}
	return s.cfg.Prefix + key, nil
}

// lifetime converts the requested expiry to a SigV4 lifetime. SigV4 has
// one-second resolution and the instruction must never outlive the requested
// instant (the attempt's authority bound) or the configured cap, so the lifetime
// is rounded down; less than one second remaining is refused.
func (s *Store) lifetime(expires time.Time) (time.Duration, error) {
	d := expires.Sub(s.now())
	if d > s.cfg.MaxTransfer {
		d = s.cfg.MaxTransfer
	}
	d = d.Truncate(time.Second)
	if d < time.Second {
		return 0, fmt.Errorf("%w: less than one second of transfer lifetime remains", artifacts.ErrInvalid)
	}
	return d, nil
}

func (s *Store) checkScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && !(s.cfg.AllowInsecure && u.Scheme == "http") {
		return errors.New("artifacts/s3: transfer endpoint must use https")
	}
	return nil
}

// Delete removes the object; an absent object is success.
func (s *Store) Delete(ctx context.Context, key string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	_, err = s.cfg.Client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(k)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("%w: delete object", artifacts.ErrUnavailable)
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	var nk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nk) {
		return true
	}
	// Provider API errors expose a stable code; matching the interface keeps the
	// adapter free of a direct dependency on the SDK's error package.
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
