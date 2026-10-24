package root_test

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

type memStore struct{ set root.ContextSet }

func newMem() *memStore { return &memStore{set: root.NewContextSet()} }

func (m *memStore) Load(context.Context) (root.ContextSet, error) {
	c := root.NewContextSet()
	c.Current = m.set.Current
	for k, v := range m.set.Contexts {
		c.Contexts[k] = v
	}
	return c, nil
}

func (m *memStore) Update(ctx context.Context, fn func(*root.ContextSet) error) error {
	s, _ := m.Load(ctx)
	if err := fn(&s); err != nil {
		return err
	}
	m.set = s
	return nil
}

type tokenResolver struct {
	src client.AccessTokenSource
	got []root.EffectiveContext
}

func (r *tokenResolver) TokenSource(ec root.EffectiveContext) (client.AccessTokenSource, error) {
	r.got = append(r.got, ec)
	return r.src, nil
}

func newRunner(t *testing.T, cfg root.RunnerConfig, c ...root.Contribution) (*root.Runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	cfg.Streams = root.Streams{Out: &out, Err: &errb}
	r, err := root.NewRunner(cfg, c...)
	if err != nil {
		t.Fatal(err)
	}
	return r, &out, &errb
}

func fileStore(t *testing.T) *root.FileContextStore {
	return root.NewFileContextStore(filepath.Join(t.TempDir(), "kodama", "contexts.json"))
}

// stalledTLSServer accepts TCP connections and never answers, so a TLS client
// waits for the server hello until its handshake timeout fires. It returns the
// https origin.
func stalledTLSServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "https://" + l.Addr().String()
}
