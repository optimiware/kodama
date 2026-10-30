package entrypoint

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func TestArtifactFilesAreDownloadedVerifiedAndPrivate(t *testing.T) {
	payload := []byte("verified bundle")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	digest := runtimev1.Digest("sha256:" + sha256Sum(payload))

	u := newUnit(t, `test "$(cat "$KODAMA_INPUTS_DIR/bundle")" = "verified bundle"`)
	u.rt.ctx.ArtifactFiles = []runtimev1.ArtifactInput{{Name: "bundle", ArtifactID: "art_bundle", Digest: digest}}
	u.rt.download = func(id domain.ArtifactID) (runtimev1.ArtifactReadResponse, error) {
		if id != "art_bundle" {
			t.Fatalf("unexpected artifact id %s", id)
		}
		return runtimev1.ArtifactReadResponse{
			ArtifactID: id, Digest: digest,
			Download: runtimev1.Upload{Method: http.MethodGet, URL: srv.URL, Headers: map[string]string{"X-Test": "yes"}, ExpiresAt: runtimev1.Timestamp(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))},
		}, nil
	}
	u.wantExit(u.run(), ExitOK)
	fi, err := os.Stat(filepath.Join(u.cfg.InputsDir, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("input mode = %v", fi.Mode().Perm())
	}
}

func sha256Sum(data []byte) string {
	h := sha256.New()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
