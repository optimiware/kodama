package s3_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	s3support "github.com/optimiware/kodama/test/support/s3"
)

func writeSecrets(t *testing.T, access, secret string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]string{"s3-access-key": access, "s3-secret-key": secret} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestConfigFromEnv(t *testing.T) {
	for _, k := range []string{s3support.EndpointEnv, s3support.BucketEnv, s3support.PrefixEnv, s3support.SecretsDirEnv} {
		t.Setenv(k, "")
	}
	if _, ok, err := s3support.ConfigFromEnv(); ok || err != nil {
		t.Fatalf("an unset endpoint is simply not configured: %v %v", ok, err)
	}
	t.Setenv(s3support.EndpointEnv, "http://127.0.0.1:4566")
	if _, ok, err := s3support.ConfigFromEnv(); !ok || err == nil {
		t.Fatal("an endpoint without a bucket is a configuration error")
	}
	t.Setenv(s3support.BucketEnv, "kodama-x")
	t.Setenv(s3support.SecretsDirEnv, t.TempDir())
	if _, _, err := s3support.ConfigFromEnv(); err == nil {
		t.Fatal("missing secret files must be reported")
	}
	t.Setenv(s3support.SecretsDirEnv, writeSecrets(t, "test\n", "  hex-secret \n"))
	t.Setenv(s3support.PrefixEnv, "scope")
	cfg, ok, err := s3support.ConfigFromEnv()
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if cfg.Prefix != "scope/" || cfg.AccessKey != "test" || cfg.SecretKey != "hex-secret" || cfg.Bucket != "kodama-x" {
		t.Fatalf("config: %+v", cfg)
	}
	for _, bad := range []string{"/abs", "a/../b"} {
		t.Setenv(s3support.PrefixEnv, bad)
		if _, _, err := s3support.ConfigFromEnv(); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	t.Setenv(s3support.PrefixEnv, "")
	t.Setenv(s3support.SecretsDirEnv, writeSecrets(t, "test", " "))
	if _, _, err := s3support.ConfigFromEnv(); err == nil {
		t.Fatal("an empty secret must be rejected")
	}
}

func TestCleanPrefixRefusesUnsafePrefixes(t *testing.T) {
	c := awss3.New(awss3.Options{Region: "us-east-1"})
	for _, p := range []string{"", "t015", "t015/x"} {
		if err := s3support.CleanPrefix(context.Background(), c, "b", p); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("prefix %q: %v", p, err)
		}
	}
}

// TestIsolatedPrefix runs against the real fixture: each test gets its own
// subprefix and cleanup removes only that subprefix.
func TestIsolatedPrefix(t *testing.T) {
	a := s3support.New(t)
	b := s3support.New(t)
	if a.Prefix == b.Prefix || !strings.HasSuffix(a.Prefix, "/") || !strings.HasPrefix(a.Prefix, a.Config.Prefix) {
		t.Fatalf("prefixes must be unique and nested in the scope prefix: %q %q", a.Prefix, b.Prefix)
	}
	if resource := os.Getenv(s3support.ResourceEnv); resource != "" && !strings.Contains(a.Prefix, resource+"-") {
		t.Fatalf("the task resource prefix %q must name the subprefix: %q", resource, a.Prefix)
	}
	ctx := context.Background()
	put := func(e *s3support.Env, key string) {
		t.Helper()
		if _, err := e.Client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(e.Bucket()), Key: aws.String(key), Body: strings.NewReader("x")}); err != nil {
			t.Fatal(err)
		}
	}
	put(a, a.Prefix+"one")
	put(a, a.Prefix+"nested/two")
	put(b, b.Prefix+"keep")
	// A sibling of the scope prefix that no test owns.
	sibling := a.Config.Prefix + "t015-foreign-" + time.Now().Format("150405.000000") + "/keep"
	put(a, sibling)
	defer func() {
		_, _ = a.Client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(a.Bucket()), Key: aws.String(sibling)})
	}()

	keys, err := a.Keys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("a sees only its own prefix: %v %v", keys, err)
	}
	a.Clean()
	if keys, _ := a.Keys(ctx); len(keys) != 0 {
		t.Fatalf("cleanup left %v", keys)
	}
	if keys, _ := b.Keys(ctx); len(keys) != 1 {
		t.Fatalf("cleanup of one test touched another: %v", keys)
	}
	if _, err := a.Client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(a.Bucket()), Key: aws.String(sibling)}); err != nil {
		t.Fatalf("cleanup deleted a foreign object: %v", err)
	}
	if err := s3support.EnsureBucket(ctx, a.Client, a.Bucket()); err != nil {
		t.Fatalf("EnsureBucket on an existing bucket: %v", err)
	}
	if err := s3support.CleanPrefix(ctx, a.Client, a.Bucket(), "does-not-end-with-slash"); err == nil {
		t.Fatal("unsafe prefix")
	}
}
