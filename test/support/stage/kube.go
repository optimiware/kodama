package stage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kube runs kubectl and helm against the stage cluster. Both are used the way an operator uses them: the
// qualification asserts what the cluster reports, not what a client library infers.
type Kube struct{ Kubeconfig string }

// Run runs kubectl and returns its standard output. Standard error is part of the error.
func (k Kube) Run(ctx context.Context, args ...string) (string, error) {
	return k.RunInput(ctx, nil, args...)
}

// RunInput runs kubectl with standard input.
func (k Kube) RunInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.Kubeconfig)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// JSON runs `kubectl get ... -o json` style commands and decodes the answer.
func (k Kube) JSON(ctx context.Context, v any, args ...string) error {
	out, err := k.Run(ctx, append(args, "-o", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), v)
}

// Exists reports whether a named object exists. Any failure other than "not found" is an error.
func (k Kube) Exists(ctx context.Context, kind, name, namespace string) (bool, error) {
	args := []string{"get", kind, name, "-o", "name"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	_, err := k.Run(ctx, args...)
	switch {
	case err == nil:
		return true, nil
	case strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found"):
		return false, nil
	}
	return false, err
}

// SecretValue reads one key of a Secret. The value is returned to the caller only; it is never logged.
func (k Kube) SecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	out, err := k.Run(ctx, "-n", namespace, "get", "secret", name, "-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", "\\.")+"}")
	if err != nil {
		return "", err
	}
	b, err := decodeBase64(out)
	return string(b), err
}

// Helm runs helm with the operator kubeconfig.
func (k Kube) Helm(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.Kubeconfig)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("helm %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// PortForward is a kubectl port-forward kept alive for the life of a test process: when kubectl exits (a Pod
// replaced, a connection dropped) it is started again on the same local port.
type PortForward struct {
	Port int

	kube      Kube
	namespace string
	target    string
	remote    int

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var forwardingLine = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// Forward starts a port forward to svc/<name> and waits until it accepts connections.
func (k Kube) Forward(ctx context.Context, namespace, target string, remote int) (*PortForward, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	pf := &PortForward{Port: port, kube: k, namespace: namespace, target: target, remote: remote, done: make(chan struct{})}
	runCtx, cancel := context.WithCancel(context.Background())
	pf.cancel = cancel
	go pf.supervise(runCtx)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
			_ = c.Close()
			return pf, nil
		}
		select {
		case <-ctx.Done():
			pf.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	pf.Close()
	return nil, errors.New("port forward to " + target + " did not become ready")
}

func (pf *PortForward) supervise(ctx context.Context) {
	defer close(pf.done)
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "kubectl", "-n", pf.namespace, "port-forward", "--address", "127.0.0.1", pf.target, strconv.Itoa(pf.Port)+":"+strconv.Itoa(pf.remote))
		cmd.Env = append(os.Environ(), "KUBECONFIG="+pf.kube.Kubeconfig)
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = nil
		if err := cmd.Start(); err == nil {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
			}
			_ = cmd.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Close stops the forward.
func (pf *PortForward) Close() {
	pf.mu.Lock()
	cancel := pf.cancel
	pf.cancel = nil
	pf.mu.Unlock()
	if cancel != nil {
		cancel()
		<-pf.done
	}
}
