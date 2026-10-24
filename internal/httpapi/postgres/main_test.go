package postgres_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// postgresImage is the pinned image the development environment also uses.
const postgresImage = "docker.io/library/postgres:17.6"

// TestMain makes the real-PostgreSQL tests runnable without manual setup. When
// KODAMA_TEST_POSTGRES_ADMIN_URL is unset it starts one throwaway loopback
// container (podman or docker), named after the task resource prefix, points
// the shared helper at it and removes it when the package finishes. An
// explicitly configured URL is always used as is, and when no container runtime
// is available the tests keep the helper's skip/require behaviour.
func TestMain(m *testing.M) {
	stop := func() {}
	if pgtest.AdminURL() == "" && !layer.Unit {
		if url, cleanup, err := startContainer(); err != nil {
			fmt.Fprintln(os.Stderr, "httpapi/postgres: no PostgreSQL provisioned:", err)
		} else {
			stop = cleanup
			os.Setenv(pgtest.AdminURLEnv, url)
		}
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func startContainer() (url string, cleanup func(), err error) {
	var runtime string
	for _, candidate := range []string{"podman", "docker"} {
		if path, lookErr := exec.LookPath(candidate); lookErr == nil {
			if exec.Command(path, "info").Run() == nil {
				runtime = path
				break
			}
		}
	}
	if runtime == "" {
		return "", nil, fmt.Errorf("no running podman or docker found")
	}
	prefix := os.Getenv("KODAMA_RESOURCE_PREFIX")
	if prefix == "" {
		prefix = "t004"
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", nil, err
	}
	name := prefix + "-pgtest-" + hex.EncodeToString(random[:4])
	password := hex.EncodeToString(random)
	out, err := exec.Command(runtime, "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD="+password,
		"-p", "127.0.0.1::5432", postgresImage,
		"-c", "max_connections=300", "-c", "fsync=off").CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("start %s container: %v: %s", runtime, err, strings.TrimSpace(string(out)))
	}
	cleanup = func() { _ = exec.Command(runtime, "rm", "-f", name).Run() }
	portOut, err := exec.Command(runtime, "port", name, "5432/tcp").Output()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("find published port: %v", err)
	}
	address := strings.TrimSpace(strings.SplitN(string(portOut), "\n", 2)[0])
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("parse port %q: %v", address, err)
	}
	// The image restarts once while initialising, so require a real query.
	deadline := time.Now().Add(90 * time.Second)
	for {
		check := exec.Command(runtime, "exec", name, "psql", "-U", "postgres", "-tAc", "select 1")
		if got, err := check.Output(); err == nil && strings.TrimSpace(string(got)) == "1" {
			if conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); dialErr == nil {
				conn.Close()
				break
			}
		}
		if time.Now().After(deadline) {
			cleanup()
			return "", nil, fmt.Errorf("PostgreSQL did not become ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "postgres://postgres:" + password + "@127.0.0.1:" + port + "/postgres?sslmode=disable", cleanup, nil
}
