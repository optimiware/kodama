package s3

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Self-provisioned fixture.
//
// When KODAMA_S3_ENDPOINT is unset (for example under `make test-task`, whose
// environment carries only the PostgreSQL and NATS fixtures) New starts, or
// reuses, one LocalStack container named "<resource prefix>-s3-localstack"
// through the same container provider the development environment uses
// (DEV_PROVIDER; podman by default on every OS). The container is the only
// resource created; it is bound to 127.0.0.1 on an ephemeral port, runs the
// image pinned in dev/compose.yaml with signature validation enabled so
// presigned-URL enforcement is really exercised, and is left running for later
// runs. Remove it with `<provider> rm -f <prefix>-s3-localstack`.
//
// KODAMA_S3_PROVISION=0 disables this and restores skip-when-unset. The
// bucket is "<prefix>-artifacts", the scope prefix "<prefix>/" and the
// credentials LocalStack's fixed development pair; none of them are secrets.
const (
	// ProvisionEnv set to "0" disables the self-provisioned fixture.
	ProvisionEnv = "KODAMA_S3_PROVISION"
	// ProviderEnv names the container provider, as in the development environment.
	ProviderEnv = "DEV_PROVIDER"

	// LocalStackImage is the digest-pinned image of dev/compose.yaml.
	LocalStackImage = "docker.io/localstack/localstack:4.14.0@sha256:3ebc37595918b8accb852f8048fef2aff047d465167edd655528065b07bc364a"

	provisionedKey = "test"
)

var (
	resourcePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

	provisionOnce sync.Once
	provisioned   Config
	provisionErr  error
)

// provisionedConfig returns the self-provisioned fixture. ok is false when
// provisioning is disabled or not possible on this host (no container
// provider); err means it was attempted and failed.
func provisionedConfig() (cfg Config, ok bool, err error) {
	if os.Getenv(ProvisionEnv) == "0" {
		return Config{}, false, nil
	}
	resource := os.Getenv(ResourceEnv)
	if !resourcePattern.MatchString(resource) {
		return Config{}, false, nil
	}
	provider := os.Getenv(ProviderEnv)
	if provider == "" {
		provider = "podman"
	}
	if provider != "podman" && provider != "docker" {
		return Config{}, false, nil
	}
	bin, lookErr := exec.LookPath(provider)
	if lookErr != nil {
		return Config{}, false, nil
	}
	provisionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		provisioned, provisionErr = startLocalStack(ctx, bin, resource)
	})
	return provisioned, provisionErr == nil, provisionErr
}

func startLocalStack(ctx context.Context, bin, resource string) (Config, error) {
	name := resource + "-s3-localstack"
	// Several test processes (one per package) race to create the container;
	// serialize creation across processes with an advisory file lock.
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Config{}, fmt.Errorf("lock fixture creation: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Config{}, fmt.Errorf("lock fixture creation: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	running, exists := containerState(ctx, bin, name)
	switch {
	case !exists:
		out, err := runCmd(ctx, bin, "run", "-d", "--name", name,
			"-p", "127.0.0.1::4566",
			"-e", "SERVICES=s3",
			"-e", "EAGER_SERVICE_LOADING=1",
			"-e", "AWS_DEFAULT_REGION="+Region,
			"-e", "S3_SKIP_SIGNATURE_VALIDATION=0",
			LocalStackImage)
		if err != nil {
			// A concurrent creator under another lock holder is the only
			// benign cause; verify by looking again.
			if _, nowExists := containerState(ctx, bin, name); !nowExists {
				return Config{}, fmt.Errorf("start LocalStack fixture %s: %w: %s", name, err, out)
			}
		}
	case !running:
		if out, err := runCmd(ctx, bin, "start", name); err != nil {
			return Config{}, fmt.Errorf("restart LocalStack fixture %s: %w: %s", name, err, out)
		}
	}
	hostPort, err := publishedPort(ctx, bin, name)
	if err != nil {
		return Config{}, err
	}
	endpoint := "http://" + hostPort
	if err := waitHealthy(ctx, endpoint); err != nil {
		return Config{}, fmt.Errorf("LocalStack fixture %s: %w", name, err)
	}
	return Config{
		Endpoint:  endpoint,
		Bucket:    resource + "-artifacts",
		Prefix:    resource + "/",
		AccessKey: provisionedKey,
		SecretKey: provisionedKey,
	}, nil
}

func containerState(ctx context.Context, bin, name string) (running, exists bool) {
	out, err := runCmd(ctx, bin, "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(out) == "true", true
}

func publishedPort(ctx context.Context, bin, name string) (string, error) {
	var last string
	for i := 0; i < 30; i++ {
		out, err := runCmd(ctx, bin, "port", name, "4566/tcp")
		last = strings.TrimSpace(out)
		if err == nil {
			for _, line := range strings.Split(last, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "127.0.0.1:") {
					return line, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("LocalStack fixture %s has no published 127.0.0.1 port: %q", name, last)
}

func waitHealthy(ctx context.Context, endpoint string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/_localstack/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy: %v: %w", last, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func runCmd(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}
