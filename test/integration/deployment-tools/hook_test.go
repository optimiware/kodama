package deploymenttools_test

// A deterministic interleaving point between the delete script's observation of an
// object and its delete. A wrapper placed first on PATH (inside the container)
// pauses every `kubectl delete --raw` and tells the test which URL it is about to
// call; the test changes the cluster while the script is paused, then releases
// it. The wrapper then runs the REAL kubectl with the script's own arguments, so
// the delete itself — preconditions included — is the real thing.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const hookWrapper = `#!/bin/sh
# Test wrapper (task T-065): pause "kubectl delete --raw <url>" until the test releases it.
url=""
del=0
prev=""
for a in "$@"; do
  if [ "$a" = "delete" ]; then del=1; fi
  if [ "$prev" = "--raw" ]; then url=$a; fi
  prev=$a
done
if [ "$del" = 1 ] && [ -n "$url" ]; then
  n=$(ls /hook | grep -c '^ready\.[0-9]*$')
  n=$((n + 1))
  printf '%s\n' "$url" > "/hook/ready.$n.tmp" && mv "/hook/ready.$n.tmp" "/hook/ready.$n"
  echo "hook: delete $n of $url paused until the test releases it" >&2
  i=0
  while [ ! -e "/hook/go.$n" ]; do
    sleep 0.1
    i=$((i + 1))
    if [ "$i" -gt 900 ]; then echo "hook: the test did not release delete $n" >&2; exit 97; fi
  done
fi
exec /usr/local/bin/kubectl "$@"
`

type deleteHook struct {
	t    *testing.T
	dir  string
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	seen []string
}

// hookDeletes returns the podman arguments and environment that put the wrapper in
// front of kubectl, and starts the goroutine that calls before(n, url) whenever the
// script is paused at a delete and then releases it. Call stop when the step ended.
func (w *world) hookDeletes(before func(n int, url string)) (args []string, env map[string]string, h *deleteHook) {
	t := w.t
	t.Helper()
	dir, err := os.MkdirTemp(w.dir, "hook-")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "kubectl-wrapper")
	if err := os.WriteFile(wrapper, []byte(hookWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	h = &deleteHook{t: t, dir: shared, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		for n := 1; ; {
			select {
			case <-h.stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			raw, err := os.ReadFile(filepath.Join(shared, "ready."+strconv.Itoa(n)))
			if err != nil {
				continue
			}
			url := strings.TrimSpace(string(raw))
			h.mu.Lock()
			h.seen = append(h.seen, url)
			h.mu.Unlock()
			before(n, url)
			if err := os.WriteFile(filepath.Join(shared, "go."+strconv.Itoa(n)), nil, 0o600); err != nil {
				t.Errorf("releasing delete %d: %v", n, err)
				return
			}
			n++
		}
	}()
	args = []string{"-v", wrapper + ":/opt/hook/kubectl:ro", "-v", shared + ":/hook:rw"}
	env = map[string]string{"PATH": "/opt/hook:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	return args, env, h
}

func (h *deleteHook) finish() {
	close(h.stop)
	<-h.done
}

// urls returns the delete URLs the script reached, in order.
func (h *deleteHook) urls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *deleteHook) count(suffix string) int {
	n := 0
	for _, u := range h.urls() {
		if strings.HasSuffix(u, suffix) {
			n++
		}
	}
	return n
}
