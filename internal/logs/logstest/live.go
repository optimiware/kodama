package logstest

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/logs"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// FakeLive is a controllable logs.LiveReader for tests that need faults the
// Kubernetes fake would only emulate (a missing workload, an outage, a stream
// that fails midway). It records the targets and queries it was asked for.
type FakeLive struct {
	mu      sync.Mutex
	Body    string
	Err     error
	FailAt  int // > 0: the stream fails with FailErr after this many bytes
	FailErr error
	targets []logs.Target
	queries []logs.LiveQuery
	opened  int
	closed  int
}

// Open implements logs.LiveReader.
func (f *FakeLive) Open(ctx context.Context, t logs.Target, q logs.LiveQuery) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets, f.queries = append(f.targets, t), append(f.queries, q)
	if f.Err != nil {
		return nil, f.Err
	}
	f.opened++
	body := []byte(f.Body)
	var r io.Reader = bytes.NewReader(body)
	if f.FailAt > 0 && f.FailAt < len(body) {
		r = io.MultiReader(bytes.NewReader(body[:f.FailAt]), errReader{f.FailErr})
	}
	return &tracked{Reader: r, f: f}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type tracked struct {
	io.Reader
	f    *FakeLive
	once sync.Once
}

func (t *tracked) Close() error {
	t.once.Do(func() {
		t.f.mu.Lock()
		t.f.closed++
		t.f.mu.Unlock()
	})
	return nil
}

// Opened and Closed count streams, so a test can prove every one was closed.
func (f *FakeLive) Opened() int { f.mu.Lock(); defer f.mu.Unlock(); return f.opened }
func (f *FakeLive) Closed() int { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }

// Targets returns every target requested.
func (f *FakeLive) Targets() []logs.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logs.Target(nil), f.targets...)
}

// Queries returns every query requested.
func (f *FakeLive) Queries() []logs.LiveQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logs.LiveQuery(nil), f.queries...)
}
