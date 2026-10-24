package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/optimiware/kodama/internal/artifacts"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

func TestStoreConfiguration(t *testing.T) {
	if _, err := arts3.NewStore(arts3.Config{Bucket: "b"}); err == nil {
		t.Error("a missing client must be rejected")
	}
	client := awss3.New(awss3.Options{Region: "us-east-1"})
	for name, cfg := range map[string]arts3.Config{
		"no bucket":      {Client: client},
		"absolute":       {Client: client, Bucket: "b", Prefix: "/abs"},
		"dot segment":    {Client: client, Bucket: "b", Prefix: "a/../b"},
		"too long a cap": {Client: client, Bucket: "b", MaxTransfer: 8 * 24 * time.Hour},
	} {
		if _, err := arts3.NewStore(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	st, err := arts3.NewStore(arts3.Config{Client: client, Bucket: "b", Prefix: "scope"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Prefix() != "scope/" {
		t.Errorf("prefix not normalized: %q", st.Prefix())
	}
}

// TestStoreRefusesForeignKeysAndInsecureTransfer needs no service: keys outside
// the artifact namespace never reach the provider, and production (the default)
// refuses http transfer URLs.
func TestStoreRefusesForeignKeysAndInsecureTransfer(t *testing.T) {
	client := awss3.New(awss3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:1"), UsePathStyle: true})
	st, err := arts3.NewStore(arts3.Config{Client: client, Bucket: "b", Prefix: "scope/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"", "other/x", "artifacts/../x", "artifacts//x", "artifacts/x/", "artifacts/a b", "artifacts/é", "/artifacts/x", "scope/artifacts/x/.."} {
		if err := st.Delete(ctx, key); !errors.Is(err, artifacts.ErrInvalid) {
			t.Errorf("Delete(%q): %v", key, err)
		}
		if _, err := st.Open(ctx, key); !errors.Is(err, artifacts.ErrInvalid) {
			t.Errorf("Open(%q): %v", key, err)
		}
	}
	d := artifacts.DigestOf([]byte("x"))
	_, err = st.PresignUpload(ctx, artifacts.UploadSpec{Key: "artifacts/a/b/c/staging/n", MediaType: "text/plain", Digest: d, Size: 1, ExpiresAt: time.Now().Add(time.Minute)})
	if !errors.Is(err, artifacts.ErrUnavailable) {
		t.Errorf("an http endpoint must not be signed in production mode: %v", err)
	}
	_, err = st.PresignDownload(ctx, artifacts.DownloadSpec{Key: "artifacts/a/b/c/sealed/n", ExpiresAt: time.Now().Add(time.Minute)})
	if !errors.Is(err, artifacts.ErrUnavailable) {
		t.Errorf("an http endpoint must not be signed for download in production mode: %v", err)
	}
	if _, err := st.PresignUpload(ctx, artifacts.UploadSpec{Key: "artifacts/a/b/c/staging/n", MediaType: "", Digest: d, Size: 1, ExpiresAt: time.Now().Add(time.Minute)}); !errors.Is(err, artifacts.ErrInvalid) {
		t.Errorf("an upload needs its declaration: %v", err)
	}
}

func TestStoreByteOperations(t *testing.T) {
	env := s3support.New(t)
	st := newStore(t, env, nil)
	ctx := context.Background()
	data := []byte("sealed bytes")
	key := "artifacts/t_a/p_1/art_9/sealed/n1"

	if _, err := st.Open(ctx, key); !errors.Is(err, artifacts.ErrObjectNotFound) {
		t.Fatalf("absent object: %v", err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("deleting an absent object is success: %v", err)
	}
	put := func(body []byte, digest artifacts.Digest) error {
		return st.Put(ctx, artifacts.PutSpec{Key: key, Body: bytes.NewReader(body), Size: int64(len(body)), Digest: digest, MediaType: "text/plain"})
	}
	if err := put([]byte("different"), artifacts.DigestOf(data)); err == nil {
		t.Fatal("bytes that do not match the checksum were stored")
	}
	if _, err := st.Open(ctx, key); !errors.Is(err, artifacts.ErrObjectNotFound) {
		t.Fatalf("a rejected write must store nothing: %v", err)
	}
	if err := put(data, artifacts.DigestOf(data)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := put([]byte("replacement"), artifacts.DigestOf([]byte("replacement"))); err == nil {
		t.Fatal("a sealed destination must never be replaced")
	}
	obj, err := st.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("sealed content changed: %q", got)
	}
	keys, err := env.Keys(ctx)
	if err != nil || len(keys) != 1 || !strings.HasPrefix(keys[0], env.Prefix+"artifacts/") {
		t.Fatalf("objects must live under the owned prefix: %v %v", keys, err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Open(ctx, key); !errors.Is(err, artifacts.ErrObjectNotFound) {
		t.Fatalf("deleted object still readable: %v", err)
	}
}
