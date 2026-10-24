package artifactstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
)

// ObjectsEnv is an artifacts.ObjectStore with the means to act as the holder of
// a transfer instruction (a workload or a reader).
type ObjectsEnv struct {
	Store artifacts.ObjectStore
	// Upload performs a PUT instruction with data and returns the HTTP status.
	Upload func(artifacts.Transfer, []byte) int
	// Download performs a GET instruction and returns the status and the body.
	Download func(artifacts.Transfer) (int, []byte)
}

// OpenObjects builds an empty ObjectsEnv.
type OpenObjects func(testing.TB) ObjectsEnv

// ObjectContract is the behavior every artifacts.ObjectStore adapter shares:
// sealed bytes are written once and only when they are the declared content, an
// absent key is ErrObjectNotFound and deleting it succeeds, keys outside the
// artifact namespace never reach the store, and the issued transfer instructions
// bind the declared content. It asserts outcomes, not signing details.
func ObjectContract(t *testing.T, open OpenObjects) {
	t.Helper()
	ctx := context.Background()
	var n int
	key := func(segment string) string {
		n++
		return fmt.Sprintf("artifacts/t088/p088/art_t088/%s/n%d", segment, n)
	}
	spec := func(k string, data []byte) artifacts.PutSpec {
		return artifacts.PutSpec{Key: k, Body: bytes.NewReader(data), Size: int64(len(data)), Digest: artifacts.DigestOf(data), MediaType: "text/plain"}
	}
	read := func(t *testing.T, st artifacts.ObjectStore, k string) ([]byte, int64, error) {
		t.Helper()
		obj, err := st.Open(ctx, k)
		if err != nil {
			return nil, 0, err
		}
		defer obj.Body.Close()
		b, err := io.ReadAll(obj.Body)
		return b, obj.Size, err
	}

	t.Run("sealed bytes are written once and read back", func(t *testing.T) {
		st := open(t).Store
		k, data := key("sealed"), []byte("sealed bytes")
		if _, _, err := read(t, st, k); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("open before put: %v", err)
		}
		if err := st.Put(ctx, spec(k, data)); err != nil {
			t.Fatalf("put: %v", err)
		}
		got, size, err := read(t, st, k)
		if err != nil || !bytes.Equal(got, data) || (size >= 0 && size != int64(len(data))) {
			t.Fatalf("open: %q size %d %v", got, size, err)
		}
		// A sealed destination is never replaced, even with identical bytes.
		for _, again := range [][]byte{[]byte("other bytes!!"), data} {
			if err := st.Put(ctx, spec(k, again)); err == nil {
				t.Fatalf("an existing key was written again with %q", again)
			}
		}
		if got, _, err := read(t, st, k); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("a refused put changed the object: %q %v", got, err)
		}
		if err := st.Delete(ctx, k); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, _, err := read(t, st, k); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("open after delete: %v", err)
		}
		if err := st.Delete(ctx, k); err != nil {
			t.Fatalf("deleting an absent key must succeed: %v", err)
		}
		if err := st.Put(ctx, spec(k, []byte("fresh"))); err != nil {
			t.Fatalf("a deleted key is free again: %v", err)
		}
	})

	t.Run("bytes that are not the declared content are refused", func(t *testing.T) {
		st := open(t).Store
		k := key("sealed")
		data := []byte("declared")
		bad := spec(k, data)
		bad.Digest = artifacts.DigestOf([]byte("something else"))
		if err := st.Put(ctx, bad); err == nil {
			t.Fatal("bytes with the wrong digest were stored")
		}
		short := spec(k, data)
		short.Size = int64(len(data)) + 1
		if err := st.Put(ctx, short); err == nil {
			t.Fatal("bytes with the wrong size were stored")
		}
		if _, _, err := read(t, st, k); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("a refused put left an object: %v", err)
		}
	})

	t.Run("keys outside the artifact namespace never reach the store", func(t *testing.T) {
		env := open(t)
		d := artifacts.DigestOf([]byte("x"))
		for _, k := range []string{"", "other/x", "artifacts/../x", "artifacts//x", "artifacts/x/", "artifacts/a b", "artifacts/é", "/artifacts/x"} {
			if _, _, err := read(t, env.Store, k); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("Open(%q): %v", k, err)
			}
			if err := env.Store.Delete(ctx, k); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("Delete(%q): %v", k, err)
			}
			if err := env.Store.Put(ctx, spec(k, []byte("x"))); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("Put(%q): %v", k, err)
			}
			if _, err := env.Store.PresignUpload(ctx, artifacts.UploadSpec{Key: k, MediaType: "text/plain", Digest: d, Size: 1, ExpiresAt: time.Now().Add(time.Minute)}); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("PresignUpload(%q): %v", k, err)
			}
			if _, err := env.Store.PresignDownload(ctx, artifacts.DownloadSpec{Key: k, ExpiresAt: time.Now().Add(time.Minute)}); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("PresignDownload(%q): %v", k, err)
			}
		}
	})

	t.Run("transfer instructions need a declaration and a lifetime", func(t *testing.T) {
		st := open(t).Store
		k, d := key("staging"), artifacts.DigestOf([]byte("x"))
		for name, up := range map[string]artifacts.UploadSpec{
			"no media type": {Key: k, Digest: d, Size: 1, ExpiresAt: time.Now().Add(time.Minute)},
			"bad digest":    {Key: k, MediaType: "text/plain", Digest: "sha256:zz", Size: 1, ExpiresAt: time.Now().Add(time.Minute)},
			"negative size": {Key: k, MediaType: "text/plain", Digest: d, Size: -1, ExpiresAt: time.Now().Add(time.Minute)},
			"expired":       {Key: k, MediaType: "text/plain", Digest: d, Size: 1, ExpiresAt: time.Now().Add(-time.Minute)},
			"too short":     {Key: k, MediaType: "text/plain", Digest: d, Size: 1, ExpiresAt: time.Now().Add(200 * time.Millisecond)},
		} {
			if _, err := st.PresignUpload(ctx, up); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := st.PresignDownload(ctx, artifacts.DownloadSpec{Key: key("sealed"), ExpiresAt: time.Now().Add(-time.Minute)}); !errors.Is(err, artifacts.ErrInvalid) {
			t.Errorf("an expired download: %v", err)
		}
	})

	t.Run("an upload instruction binds the declared content", func(t *testing.T) {
		env := open(t)
		k, data := key("staging"), []byte("uploaded by a workload")
		up, err := env.Store.PresignUpload(ctx, artifacts.UploadSpec{Key: k, MediaType: "text/plain", Digest: artifacts.DigestOf(data), Size: int64(len(data)), ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil || up.Method != "PUT" || up.URL == "" || up.ExpiresAt.IsZero() {
			t.Fatalf("presign: %+v %v", up, err)
		}
		// The same length with other bytes is not the declaration.
		if code := env.Upload(up, bytes.Repeat([]byte("x"), len(data))); code/100 == 2 {
			t.Fatalf("different bytes were accepted (%d)", code)
		}
		if _, _, err := read(t, env.Store, k); !errors.Is(err, artifacts.ErrObjectNotFound) {
			t.Fatalf("a refused upload left an object: %v", err)
		}
		if code := env.Upload(up, data); code != 200 {
			t.Fatalf("upload: %d", code)
		}
		if got, _, err := read(t, env.Store, k); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("uploaded object: %q %v", got, err)
		}
	})

	t.Run("a download instruction serves one sealed key", func(t *testing.T) {
		env := open(t)
		k, data := key("sealed"), []byte("download me")
		if err := env.Store.Put(ctx, spec(k, data)); err != nil {
			t.Fatal(err)
		}
		dl, err := env.Store.PresignDownload(ctx, artifacts.DownloadSpec{Key: k, MediaType: "text/plain", ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil || dl.Method != "GET" || dl.URL == "" {
			t.Fatalf("presign: %+v %v", dl, err)
		}
		if code, body := env.Download(dl); code != 200 || !bytes.Equal(body, data) {
			t.Fatalf("download: %d %q", code, body)
		}
		gone, err := env.Store.PresignDownload(ctx, artifacts.DownloadSpec{Key: key("sealed"), ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := env.Download(gone); code == 200 {
			t.Fatal("a missing object was served")
		}
	})
}

// OpenMemObjects is the OpenObjects of the in-memory store, with its loopback
// transfer endpoint.
func OpenMemObjects(t testing.TB) ObjectsEnv {
	t.Helper()
	m := NewMemObjects(nil)
	if _, err := m.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return ObjectsEnv{Store: m, Upload: httpUpload, Download: httpDownload}
}

// OpenS3Objects is the OpenObjects of the S3 adapter on the fixture's LocalStack
// bucket. It skips without S3, and always in the unit layer.
func OpenS3Objects(t testing.TB) ObjectsEnv {
	t.Helper()
	e := New(t, Options{})
	return ObjectsEnv{
		Store: e.Objects,
		Upload: func(tr artifacts.Transfer, data []byte) int {
			code, _ := e.Upload(tr, data)
			return code
		},
		Download: e.Download,
	}
}
