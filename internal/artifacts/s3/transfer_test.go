package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func newStore(t *testing.T, env *s3support.Env, mutate func(*arts3.Config)) *arts3.Store {
	t.Helper()
	cfg := arts3.Config{Client: env.Client, Bucket: env.Bucket(), Prefix: env.Prefix, AllowInsecure: true}
	if mutate != nil {
		mutate(&cfg)
	}
	st, err := arts3.NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func send(t *testing.T, method, target string, headers map[string]string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(b)
}

func uploadSpec(key string, data []byte, expires time.Time) artifacts.UploadSpec {
	return artifacts.UploadSpec{Key: key, MediaType: "text/plain", Digest: artifacts.DigestOf(data), Size: int64(len(data)), ExpiresAt: expires}
}

// Test_expired_url qualifies the provider's actual enforcement of signed
// transfer instructions against real S3-compatible storage: lifetime, method
// and key binding, checksum binding, renewal. Provider behavior that depends on
// configuration is characterized, not assumed: when the endpoint does not
// validate signatures (LocalStack's default S3_SKIP_SIGNATURE_VALIDATION=1) the
// enforcement subtests are skipped with that reason, and the module's own bound
// (never sign beyond the requested expiry) is still asserted.
func Test_expired_url(t *testing.T) {
	env := s3support.New(t)
	st := newStore(t, env, nil)
	ctx := context.Background()
	data := []byte("evidence bytes for the transfer test")
	key := "artifacts/t_a/p_1/art_1/staging/n1"

	t.Run("instructions work within their lifetime", func(t *testing.T) {
		up, err := st.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if up.Method != "PUT" || up.Headers["X-Amz-Checksum-Sha256"] == "" || up.Headers["Content-Type"] != "text/plain" {
			t.Fatalf("unexpected instruction: %+v", up)
		}
		if code, body := send(t, up.Method, up.URL, up.Headers, data); code != 200 {
			t.Fatalf("upload: %d %s", code, body)
		}
		obj, err := st.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Body.Close()
		got, _ := io.ReadAll(obj.Body)
		if !bytes.Equal(got, data) || obj.Size != int64(len(data)) {
			t.Fatalf("stored bytes differ")
		}
		dl, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key, MediaType: "text/plain", ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(dl.Method, dl.URL, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !bytes.Equal(b, data) {
			t.Fatalf("download: %d", resp.StatusCode)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
			t.Errorf("download is not forced to attachment: %q", cd)
		}
	})

	t.Run("signing never exceeds the requested or configured lifetime", func(t *testing.T) {
		short := newStore(t, env, func(c *arts3.Config) { c.MaxTransfer = 2 * time.Second })
		up, err := short.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(24*time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		if got := expiresParam(t, up.URL); got != 2 {
			t.Fatalf("signed for %ds despite a 2s cap", got)
		}
		// The instruction never outlives the requested instant: 2.9s becomes 2s.
		rounded, err := st.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(2900*time.Millisecond)))
		if err != nil {
			t.Fatal(err)
		}
		if got := expiresParam(t, rounded.URL); got != 2 {
			t.Fatalf("a 2.9s request must be signed for 2s, not %ds", got)
		}
		if _, err := st.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(900*time.Millisecond))); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("less than a second cannot be signed without exceeding the bound: %v", err)
		}
		if _, err := st.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(-time.Second))); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("an already expired request must not be signed: %v", err)
		}
		if _, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key, ExpiresAt: time.Now()}); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("an already expired download must not be signed: %v", err)
		}
	})

	t.Run("checksum binds the uploaded bytes", func(t *testing.T) {
		up, err := st.PresignUpload(ctx, uploadSpec(key+"-bad", data, time.Now().Add(time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := send(t, up.Method, up.URL, up.Headers, bytes.ToUpper(data)); code == 200 {
			t.Fatal("bytes that do not match the signed checksum were accepted")
		}
		if _, err := st.Open(ctx, key+"-bad"); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("nothing may be stored for a rejected upload: %v", err)
		}
	})

	t.Run("expired instructions fail", func(t *testing.T) {
		up, err := st.PresignUpload(ctx, uploadSpec(key+"-exp", data, time.Now().Add(1500*time.Millisecond)))
		if err != nil {
			t.Fatal(err)
		}
		dl, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key, ExpiresAt: time.Now().Add(1500 * time.Millisecond)})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		putCode, _ := send(t, up.Method, up.URL, up.Headers, data)
		getCode, _ := send(t, dl.Method, dl.URL, nil, nil)
		if putCode == 200 || getCode == 200 {
			if !enforcesSignatures(t, st, env) {
				t.Skipf("provider does not validate presigned URLs (put=%d get=%d); set S3_SKIP_SIGNATURE_VALIDATION=0 on LocalStack to qualify expiry", putCode, getCode)
			}
			t.Fatalf("expired instructions were honored: put=%d get=%d", putCode, getCode)
		}
		if putCode != 403 || getCode != 403 {
			t.Fatalf("expired instructions should be refused with 403: put=%d get=%d", putCode, getCode)
		}
		if _, err := st.Open(ctx, key+"-exp"); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("an expired upload must store nothing: %v", err)
		}
	})

	t.Run("method and key tampering fail", func(t *testing.T) {
		if !enforcesSignatures(t, st, env) {
			t.Skip("provider does not validate presigned URL signatures; method/key binding cannot be qualified here")
		}
		up, err := st.PresignUpload(ctx, uploadSpec(key+"-t", data, time.Now().Add(time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		other := strings.Replace(up.URL, "/staging/n1-t", "/staging/other", 1)
		if other == up.URL {
			t.Fatal("test did not alter the key")
		}
		if code, _ := send(t, "PUT", other, up.Headers, data); code == 200 {
			t.Fatal("a signed URL was accepted for another key")
		}
		dl, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key, ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := send(t, "PUT", dl.URL, nil, []byte("overwrite")); code == 200 {
			t.Fatal("a download instruction authorized a write")
		}
		if code, _ := send(t, "DELETE", dl.URL, nil, nil); code == 200 || code == 204 {
			t.Fatal("a download instruction authorized a delete")
		}
		obj, err := st.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Body.Close()
		got, _ := io.ReadAll(obj.Body)
		if !bytes.Equal(got, data) {
			t.Fatal("tampering changed stored bytes")
		}
	})

	t.Run("renewal issues a fresh instruction without extending authority", func(t *testing.T) {
		bound := time.Now().Add(1500 * time.Millisecond)
		first, err := st.PresignUpload(ctx, uploadSpec(key+"-r", data, bound))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2200 * time.Millisecond)
		// The caller's authority bound has passed: a renewal for it is refused
		// at signing, so there is nothing to extend.
		if _, err := st.PresignUpload(ctx, uploadSpec(key+"-r", data, bound)); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("renewal beyond the authority bound must not be signed: %v", err)
		}
		fresh, err := st.PresignUpload(ctx, uploadSpec(key+"-r", data, time.Now().Add(time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.URL == first.URL {
			t.Fatal("a renewal must be a new instruction")
		}
		if code, body := send(t, fresh.Method, fresh.URL, fresh.Headers, data); code != 200 {
			t.Fatalf("fresh upload: %d %s", code, body)
		}
	})
}

// enforcesSignatures reports whether the provider rejects a presigned URL whose
// key was altered; a provider that skips signature validation honors it.
func enforcesSignatures(t *testing.T, st *arts3.Store, env *s3support.Env) bool {
	t.Helper()
	ctx := context.Background()
	data := []byte("canary")
	key := "artifacts/t_a/p_1/art_canary/staging/c1"
	up, err := st.PresignUpload(ctx, uploadSpec(key, data, time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := send(t, up.Method, up.URL, up.Headers, data); code != 200 {
		t.Fatalf("canary upload failed: %d %s", code, body)
	}
	dl, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(dl.URL, "/c1?", "/c2?", 1)
	code, _ := send(t, "GET", altered, nil, nil)
	return code == 403
}

func expiresParam(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, c := range u.Query().Get("X-Amz-Expires") {
		n = n*10 + int(c-'0')
	}
	return n
}
