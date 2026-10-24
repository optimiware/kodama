package artifactstest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
)

// MemObjects is the `development` artifacts.ObjectStore: bytes in memory, keys
// confined to the artifact namespace, sealed destinations that are never
// replaced, and transfer instructions served by a loopback HTTP endpoint inside
// the process (Serve), so a workload on the Process runtime uploads and downloads
// exactly as it would against S3.
//
// It extends the minimal Open/Put/Delete store the gates tests use with the
// presigned transfers the artifact service issues. It is development-only and
// holds nothing across a restart.
type MemObjects struct {
	clock func() time.Time

	mu     sync.Mutex
	data   map[string]memObject
	grants map[string]grant
	base   string
	srv    *http.Server
}

type memObject struct {
	data      []byte
	mediaType string
}

type grant struct {
	upload    bool
	key       string
	digest    artifacts.Digest
	size      int64
	mediaType string
	expires   time.Time
}

var _ artifacts.ObjectStore = (*MemObjects)(nil)

// NewMemObjects returns an empty store. clock may be nil (the wall clock).
func NewMemObjects(clock func() time.Time) *MemObjects {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemObjects{clock: clock, data: map[string]memObject{}, grants: map[string]grant{}}
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemObjects) DevelopmentOnly() bool { return true }

// Serve starts the loopback transfer endpoint on 127.0.0.1 and returns its base
// URL. Without it, presigning is unavailable. Close stops it.
func (m *MemObjects) Serve() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv != nil {
		return m.base, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("%w: listen: %v", artifacts.ErrUnavailable, err)
	}
	m.base = "http://" + ln.Addr().String()
	srv := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second}
	m.srv = srv
	go func() { _ = srv.Serve(ln) }()
	return m.base, nil
}

// Close stops the transfer endpoint.
func (m *MemObjects) Close() error {
	m.mu.Lock()
	srv := m.srv
	m.srv, m.base = nil, ""
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// Handler serves the instructions PresignUpload and PresignDownload issue.
func (m *MemObjects) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/", m.serveUpload)
	mux.HandleFunc("/download/", m.serveDownload)
	return mux
}

func (m *MemObjects) lookup(r *http.Request, prefix string, upload bool) (grant, int) {
	token := strings.TrimPrefix(r.URL.Path, prefix)
	m.mu.Lock()
	g, ok := m.grants[token]
	m.mu.Unlock()
	switch {
	case !ok || g.upload != upload:
		return grant{}, http.StatusForbidden
	case !m.clock().Before(g.expires):
		return grant{}, http.StatusForbidden
	}
	return g, 0
}

func (m *MemObjects) serveUpload(w http.ResponseWriter, r *http.Request) {
	g, status := m.lookup(r, "/upload/", true)
	if status != 0 || r.Method != http.MethodPut {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The instruction binds the media type, the length and the content digest.
	if r.Header.Get("Content-Type") != g.mediaType || r.ContentLength != g.size {
		http.Error(w, "the request does not match the signed declaration", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, g.size+1))
	if err != nil || int64(len(body)) != g.size || artifacts.DigestOf(body) != g.digest {
		http.Error(w, "the content does not match the signed checksum", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.data[g.key] = memObject{data: body, mediaType: g.mediaType}
	m.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (m *MemObjects) serveDownload(w http.ResponseWriter, r *http.Request) {
	g, status := m.lookup(r, "/download/", false)
	if status != 0 || r.Method != http.MethodGet {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	m.mu.Lock()
	o, ok := m.data[g.key]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	mt := g.mediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	_, _ = w.Write(o.data)
}

// checkKey accepts only module-shaped keys, as the S3 adapter does.
func checkKey(key string) error {
	if !strings.HasPrefix(key, "artifacts/") || strings.Contains(key, "..") || strings.Contains(key, "//") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: object key is outside the artifact namespace", artifacts.ErrInvalid)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: object key has an unexpected character", artifacts.ErrInvalid)
		}
	}
	return nil
}

func (m *MemObjects) lifetimeOK(expires time.Time) error {
	if expires.Sub(m.clock()).Truncate(time.Second) < time.Second {
		return fmt.Errorf("%w: less than one second of transfer lifetime remains", artifacts.ErrInvalid)
	}
	return nil
}

func (m *MemObjects) issue(g grant, path string) (artifacts.Transfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base == "" {
		return artifacts.Transfer{}, fmt.Errorf("%w: the in-memory object store has no transfer endpoint; call Serve", artifacts.ErrUnavailable)
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return artifacts.Transfer{}, fmt.Errorf("%w: token: %v", artifacts.ErrUnavailable, err)
	}
	token := hex.EncodeToString(raw[:])
	m.grants[token] = g
	return artifacts.Transfer{URL: m.base + path + token, ExpiresAt: g.expires.UTC()}, nil
}

// PresignUpload signs a PUT for exactly one staging key: it binds the media
// type, the length and the content digest, and expires at the requested instant.
func (m *MemObjects) PresignUpload(_ context.Context, spec artifacts.UploadSpec) (artifacts.Transfer, error) {
	if err := checkKey(spec.Key); err != nil {
		return artifacts.Transfer{}, err
	}
	if err := spec.Digest.Validate(); err != nil || spec.Size < 0 || spec.MediaType == "" {
		return artifacts.Transfer{}, fmt.Errorf("%w: upload declaration", artifacts.ErrInvalid)
	}
	if err := m.lifetimeOK(spec.ExpiresAt); err != nil {
		return artifacts.Transfer{}, err
	}
	t, err := m.issue(grant{upload: true, key: spec.Key, digest: spec.Digest, size: spec.Size, mediaType: spec.MediaType, expires: spec.ExpiresAt}, "/upload/")
	if err != nil {
		return artifacts.Transfer{}, err
	}
	t.Method = http.MethodPut
	t.Headers = map[string]string{"Content-Type": spec.MediaType}
	return t, nil
}

// PresignDownload signs a GET for exactly one sealed key.
func (m *MemObjects) PresignDownload(_ context.Context, spec artifacts.DownloadSpec) (artifacts.Transfer, error) {
	if err := checkKey(spec.Key); err != nil {
		return artifacts.Transfer{}, err
	}
	if err := m.lifetimeOK(spec.ExpiresAt); err != nil {
		return artifacts.Transfer{}, err
	}
	t, err := m.issue(grant{key: spec.Key, mediaType: spec.MediaType, expires: spec.ExpiresAt}, "/download/")
	if err != nil {
		return artifacts.Transfer{}, err
	}
	t.Method = http.MethodGet
	t.Headers = map[string]string{}
	return t, nil
}

// Open implements artifacts.ObjectStore.
func (m *MemObjects) Open(_ context.Context, key string) (artifacts.Object, error) {
	if err := checkKey(key); err != nil {
		return artifacts.Object{}, err
	}
	m.mu.Lock()
	o, ok := m.data[key]
	m.mu.Unlock()
	if !ok {
		return artifacts.Object{}, artifacts.ErrObjectNotFound
	}
	return artifacts.Object{Body: io.NopCloser(bytes.NewReader(append([]byte(nil), o.data...))), Size: int64(len(o.data))}, nil
}

// Put writes verified bytes at an unused key: it refuses bytes that are not the
// declared content and never replaces an existing object.
func (m *MemObjects) Put(_ context.Context, spec artifacts.PutSpec) error {
	if err := checkKey(spec.Key); err != nil {
		return err
	}
	if _, err := spec.Body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: rewind body", artifacts.ErrUnavailable)
	}
	body, err := io.ReadAll(spec.Body)
	if err != nil {
		return fmt.Errorf("%w: read body", artifacts.ErrUnavailable)
	}
	if int64(len(body)) != spec.Size || artifacts.DigestOf(body) != spec.Digest {
		return fmt.Errorf("%w: write object: the bytes are not the declared content", artifacts.ErrUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.data[spec.Key]; exists {
		return fmt.Errorf("%w: write object: the key is already in use", artifacts.ErrUnavailable)
	}
	m.data[spec.Key] = memObject{data: body, mediaType: spec.MediaType}
	return nil
}

// Delete removes the object; an absent object is success.
func (m *MemObjects) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

// Keys lists the stored keys, for tests.
func (m *MemObjects) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

// Exists reports whether key holds an object.
func (m *MemObjects) Exists(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}
