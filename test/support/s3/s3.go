// Package s3 provides isolated S3 prefixes for integration tests.
//
// It reuses the development fixture contract (dev/configuration.md) without
// changing it:
//
//	KODAMA_S3_ENDPOINT   http(s) endpoint reachable from the test process
//	KODAMA_S3_BUCKET     bucket created by the fixture (created when absent)
//	KODAMA_S3_PREFIX     scope prefix; every test appends a unique subprefix
//	KODAMA_SECRETS_DIR   directory holding s3-access-key and s3-secret-key
//	                     (default /secrets, the fixture mount)
//	KODAMA_REQUIRE_S3=1  fail instead of skip when the endpoint is unset
//
// New hands a test its own subprefix "<KODAMA_S3_PREFIX><resource prefix>-<random>/"
// (the resource prefix is KODAMA_RESOURCE_PREFIX, "t015" for this task) and
// removes every object under exactly that subprefix when the test ends. It
// never lists, reads or deletes anything outside it, so concurrent tests and
// foreign data are untouched. Signatures use SigV4, region us-east-1 and
// path-style addressing like the fixture.
package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/optimiware/kodama/test/support/layer"
)

const (
	EndpointEnv   = "KODAMA_S3_ENDPOINT"
	BucketEnv     = "KODAMA_S3_BUCKET"
	PrefixEnv     = "KODAMA_S3_PREFIX"
	SecretsDirEnv = "KODAMA_SECRETS_DIR"
	RequireEnv    = "KODAMA_REQUIRE_S3"
	ResourceEnv   = "KODAMA_RESOURCE_PREFIX"

	// Region is the fixture's fixed region.
	Region = "us-east-1"
	// DefaultSecretsDir is where the fixture mounts its secret files.
	DefaultSecretsDir = "/secrets"
)

// Config is the fixture description read from the environment.
type Config struct {
	Endpoint string
	Bucket   string
	// Prefix is the scope prefix, normalized to end with "/" (or empty).
	Prefix    string
	AccessKey string
	SecretKey string
}

// ConfigFromEnv reads the fixture contract. ok is false when the endpoint is
// not configured; an error means the configuration is present but unusable.
func ConfigFromEnv() (cfg Config, ok bool, err error) {
	cfg.Endpoint = os.Getenv(EndpointEnv)
	if cfg.Endpoint == "" {
		return Config{}, false, nil
	}
	cfg.Bucket = os.Getenv(BucketEnv)
	if cfg.Bucket == "" {
		return Config{}, true, fmt.Errorf("%s is set but %s is empty", EndpointEnv, BucketEnv)
	}
	cfg.Prefix = os.Getenv(PrefixEnv)
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	if strings.HasPrefix(cfg.Prefix, "/") || strings.Contains(cfg.Prefix, "..") {
		return Config{}, true, fmt.Errorf("%s must be relative and free of dot segments", PrefixEnv)
	}
	dir := os.Getenv(SecretsDirEnv)
	if dir == "" {
		dir = DefaultSecretsDir
	}
	if cfg.AccessKey, err = readSecret(dir, "s3-access-key"); err != nil {
		return Config{}, true, err
	}
	if cfg.SecretKey, err = readSecret(dir, "s3-secret-key"); err != nil {
		return Config{}, true, err
	}
	return cfg, true, nil
}

func readSecret(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("read fixture secret %s: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("fixture secret %s is empty", name)
	}
	return v, nil
}

// NewClient builds an SDK client for the fixture: static credentials, SigV4,
// fixed region, path-style addressing.
func NewClient(cfg Config) *awss3.Client {
	return awss3.New(awss3.Options{
		Region:       Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	})
}

// Env is one test's isolated prefix.
type Env struct {
	Client *awss3.Client
	Config Config
	// Prefix is the test's own subprefix, ending with "/". Every object a test
	// creates must live under it.
	Prefix string
	t      testing.TB
}

// New returns the test's environment or skips (fails when KODAMA_REQUIRE_S3=1)
// when the S3 fixture is neither configured nor self-provisionable (see
// provision.go). Cleanup removes only objects under the
// test's subprefix.
func New(t testing.TB) *Env {
	t.Helper()
	layer.SkipService(t, "S3")
	cfg, ok, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("S3 fixture configuration: %v", err)
	}
	if !ok {
		cfg, ok, err = provisionedConfig()
		if err != nil {
			t.Fatalf("self-provisioned S3 fixture: %v", err)
		}
	}
	if !ok {
		if os.Getenv(RequireEnv) == "1" {
			t.Fatalf("%s is unset but %s=1: the S3 fixture is required", EndpointEnv, RequireEnv)
		}
		t.Skipf("S3 integration test skipped: set %s, %s, %s or make a container provider available (or %s=1 to require it)", EndpointEnv, BucketEnv, PrefixEnv, RequireEnv)
	}
	e := &Env{Client: NewClient(cfg), Config: cfg, t: t}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := EnsureBucket(ctx, e.Client, cfg.Bucket); err != nil {
		t.Fatalf("S3 fixture bucket: %v", err)
	}
	resource := os.Getenv(ResourceEnv)
	if resource == "" {
		resource = "test"
	}
	e.Prefix = cfg.Prefix + resource + "-" + randomHex(t, 6) + "/"
	t.Cleanup(func() { e.Clean() })
	return e
}

// EnsureBucket creates the bucket when it does not exist.
func EnsureBucket(ctx context.Context, c *awss3.Client, bucket string) error {
	_, err := c.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &bucket})
	if err == nil {
		return nil
	}
	var missing *types.NotFound
	if !errors.As(err, &missing) {
		// LocalStack may answer 404 with a different shape; try to create.
		if !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "404") {
			return err
		}
	}
	if _, err := c.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: &bucket}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) {
			return nil
		}
		return err
	}
	return nil
}

// Bucket returns the fixture bucket.
func (e *Env) Bucket() string { return e.Config.Bucket }

// Keys lists every object key under the test prefix.
func (e *Env) Keys(ctx context.Context) ([]string, error) {
	return ListKeys(ctx, e.Client, e.Config.Bucket, e.Prefix)
}

// ListKeys lists keys under prefix.
func ListKeys(ctx context.Context, c *awss3.Client, bucket, prefix string) ([]string, error) {
	var keys []string
	p := awss3.NewListObjectsV2Paginator(c, &awss3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

// Clean deletes every object under the test prefix and nothing else. It is
// called by t.Cleanup and may be called earlier to reset the prefix.
func (e *Env) Clean() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := CleanPrefix(ctx, e.Client, e.Config.Bucket, e.Prefix); err != nil {
		e.t.Errorf("S3 cleanup of %s: %v", e.Prefix, err)
	}
}

// CleanPrefix deletes all objects under prefix. A prefix that is empty or does
// not end with "/" is refused: cleanup must never reach a sibling.
func CleanPrefix(ctx context.Context, c *awss3.Client, bucket, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("refusing to clean prefix %q", prefix)
	}
	keys, err := ListKeys(ctx, c, bucket, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			return fmt.Errorf("listing returned %q outside %q", k, prefix)
		}
		if _, err := c.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: &bucket, Key: aws.String(k)}); err != nil {
			return err
		}
	}
	return nil
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
