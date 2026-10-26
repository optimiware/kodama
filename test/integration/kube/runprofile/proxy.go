package runprofile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// StartProxy runs `kubectl proxy` on a loopback ephemeral port and returns its
// base URL. The proxy authenticates with the explicit kubeconfig, so the
// qualifier can issue API calls kubectl cannot express, notably DELETE with
// uid/resourceVersion preconditions. It is bounded by ctx.
func (k *Kubectl) StartProxy(ctx context.Context) (string, func(), error) {
	pctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pctx, k.Binary, "--kubeconfig", k.Kubeconfig, "proxy", "--address=127.0.0.1", "--port=0")
	cmd.Env = cleanEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() { cancel(); <-exited }
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if _, rest, ok := strings.Cut(sc.Text(), "Starting to serve on "); ok {
				found <- strings.TrimSpace(rest)
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case addr := <-found:
		k.Rec.record([]string{"proxy", "--address=127.0.0.1", "--port=0"}, nil, 0, 0, "serving "+addr)
		return "http://" + addr, stop, nil
	case <-exited:
		return "", nil, fmt.Errorf("kubectl proxy exited before serving: %s", tail(errb.String(), 300))
	case <-time.After(30 * time.Second):
		stop()
		return "", nil, fmt.Errorf("kubectl proxy did not start: %s", tail(errb.String(), 300))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}

// DeleteNamespacePrecondition deletes a namespace only if the API server still
// holds the object with this UID and resourceVersion; a replacement object of
// the same name is never deleted (HTTP 409). It returns the HTTP status.
func DeleteNamespacePrecondition(ctx context.Context, hc *http.Client, base, name, uid, resourceVersion string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": uid, "resourceVersion": resourceVersion},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/v1/namespaces/"+name, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}
