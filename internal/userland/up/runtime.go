package up

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// RuntimeKind identifies the container engine speaking on a detected socket.
type RuntimeKind string

const (
	// KindPodman indicates a Podman / Libpod daemon socket.
	KindPodman RuntimeKind = "podman"
	// KindDocker indicates a Docker-compatible engine socket (Docker Desktop,
	// OrbStack, Colima or Docker Engine).
	KindDocker RuntimeKind = "docker"
)

const defaultSocketPingTimeout = 1500 * time.Millisecond

// DetectedRuntime describes a reachable container runtime socket.
type DetectedRuntime struct {
	Kind       RuntimeKind `json:"kind"`
	Provider   string      `json:"provider"`
	SocketPath string      `json:"socket_path"`
	HostAlias  string      `json:"host_alias"`
}

// DetectOptions configures container runtime socket discovery. Zero/nil fields
// use the current host's OS, home directory, environment and Unix socket probe.
type DetectOptions struct {
	// ExplicitSocket is the --socket flag value when supplied by the caller.
	ExplicitSocket string
	// Recorded is the runtime a previous `kodema up` of the instance wrote to
	// stack.json. Its socket is tried after the explicit overrides and before
	// the standard candidates, so `status`, `down` and a repeated `up` find
	// the engine that holds the instance without --socket.
	Recorded DetectedRuntime
	// GOOS overrides runtime.GOOS ("darwin", "linux", ...).
	GOOS string
	// HomeDir overrides os.UserHomeDir().
	HomeDir string
	// UID optionally supplies the numeric user ID (for /run/user/<uid>/podman/podman.sock
	// when XDG_RUNTIME_DIR is unset on Linux); a negative value disables the fallback.
	UID int
	// Getenv overrides os.Getenv.
	Getenv func(string) string
	// ReadDir overrides os.ReadDir for Podman Machine subdirectory discovery.
	ReadDir func(string) ([]os.DirEntry, error)
	// UserTempDir overrides the macOS per-user temporary directory, which
	// TMPDIR names in a login shell (getconf DARWIN_USER_TEMP_DIR).
	UserTempDir func() string
	// StatSocket reports whether path exists and resolves to a Unix domain socket.
	StatSocket func(string) bool
	// PingSocket probes http://unix/_ping on path and classifies the engine.
	PingSocket func(context.Context, string) (RuntimeKind, error)
}

type socketCandidate struct {
	provider    string
	path        string
	defaultKind RuntimeKind
}

// DetectRuntime probes explicit socket overrides (--socket, CONTAINER_HOST,
// DOCKER_HOST), then the socket the instance recorded, and then standard Linux
// and macOS container sockets in deterministic order, returning the first
// reachable Unix socket.
func DetectRuntime(ctx context.Context, opts DetectOptions) (DetectedRuntime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	goos := strings.TrimSpace(opts.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	homeDir := strings.TrimSpace(opts.HomeDir)
	if homeDir == "" {
		if envHome := strings.TrimSpace(getenv("HOME")); envHome != "" {
			homeDir = envHome
		} else if detectedHome, err := os.UserHomeDir(); err == nil {
			homeDir = detectedHome
		}
	}
	readDir := opts.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	statSocket := opts.StatSocket
	if statSocket == nil {
		statSocket = defaultStatSocket
	}
	pingSocket := opts.PingSocket
	if pingSocket == nil {
		pingSocket = defaultPingSocket
	}

	// 1. Explicit overrides (--socket, CONTAINER_HOST, DOCKER_HOST).
	for _, override := range []struct {
		source      string
		raw         string
		defaultKind RuntimeKind
	}{
		{"--socket", opts.ExplicitSocket, ""},
		{"CONTAINER_HOST", getenv("CONTAINER_HOST"), KindPodman},
		{"DOCKER_HOST", getenv("DOCKER_HOST"), KindDocker},
	} {
		raw := strings.TrimSpace(override.raw)
		if raw == "" {
			continue
		}
		socketPath, err := parseSocketEndpoint(raw, homeDir)
		if err != nil {
			return DetectedRuntime{}, fmt.Errorf("%s %q: %w", override.source, raw, err)
		}
		if !statSocket(socketPath) {
			return DetectedRuntime{}, fmt.Errorf(
				"%s socket %q does not exist or is not a Unix socket (%s)",
				override.source, socketPath, remediationForOS(goos),
			)
		}
		kind, err := pingSocket(ctx, socketPath)
		if err != nil {
			return DetectedRuntime{}, fmt.Errorf(
				"%s socket %q is not reachable: %v (%s)",
				override.source, socketPath, err, remediationForOS(goos),
			)
		}
		if kind == "" {
			kind = inferKindFromPath(socketPath, override.defaultKind)
		}
		return buildDetectedRuntime(kind, override.source, socketPath), nil
	}

	// 2. The socket the instance was started on. One that is gone or silent is
	// skipped: the engine may have moved, and the candidates below find it.
	if recorded := strings.TrimSpace(opts.Recorded.SocketPath); recorded != "" && filepath.IsAbs(recorded) && statSocket(recorded) {
		if kind, err := pingSocket(ctx, recorded); err == nil {
			if kind == "" {
				kind = opts.Recorded.Kind
			}
			provider := opts.Recorded.Provider
			if provider == "" {
				provider = "stack.json"
			}
			return buildDetectedRuntime(kind, provider, recorded), nil
		}
	}

	// 3. Standard Linux and macOS candidates in deterministic order.
	candidates := candidateSockets(goos, homeDir, getenv, opts.UID, readDir, opts.UserTempDir)
	seen := make(map[string]bool, len(candidates))
	var probed []string
	for _, candidate := range candidates {
		if candidate.path == "" || seen[candidate.path] {
			continue
		}
		seen[candidate.path] = true
		probed = append(probed, candidate.path)
		if !statSocket(candidate.path) {
			continue
		}
		kind, err := pingSocket(ctx, candidate.path)
		if err != nil {
			continue
		}
		if kind == "" {
			kind = candidate.defaultKind
		}
		return buildDetectedRuntime(kind, candidate.provider, candidate.path), nil
	}

	return DetectedRuntime{}, fmt.Errorf(
		"no reachable container runtime socket found (probed %d locations); %s",
		len(probed), remediationForOS(goos),
	)
}

// CandidateSocketPaths returns the deterministic list of standard socket paths
// that DetectRuntime probes when no explicit override is set.
func CandidateSocketPaths(opts DetectOptions) []string {
	goos := strings.TrimSpace(opts.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	homeDir := strings.TrimSpace(opts.HomeDir)
	if homeDir == "" {
		homeDir = strings.TrimSpace(getenv("HOME"))
	}
	readDir := opts.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	raw := candidateSockets(goos, homeDir, getenv, opts.UID, readDir, opts.UserTempDir)
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if c.path != "" && !seen[c.path] {
			seen[c.path] = true
			out = append(out, c.path)
		}
	}
	return out
}

func candidateSockets(
	goos string,
	homeDir string,
	getenv func(string) string,
	uid int,
	readDir func(string) ([]os.DirEntry, error),
	userTempDir func() string,
) []socketCandidate {
	var candidates []socketCandidate

	// 1. $XDG_RUNTIME_DIR/podman/podman.sock (and /run/user/<uid>/podman/podman.sock on Linux).
	if xdg := strings.TrimSpace(getenv("XDG_RUNTIME_DIR")); xdg != "" && filepath.IsAbs(xdg) {
		candidates = append(candidates, socketCandidate{
			provider:    "podman-rootless",
			path:        filepath.Join(xdg, "podman", "podman.sock"),
			defaultKind: KindPodman,
		})
	} else if goos == "linux" && uid > 0 {
		candidates = append(candidates, socketCandidate{
			provider:    "podman-rootless",
			path:        filepath.Join("/run/user", fmt.Sprintf("%d", uid), "podman", "podman.sock"),
			defaultKind: KindPodman,
		})
	}

	if homeDir != "" && filepath.IsAbs(homeDir) {
		// 2. macOS Podman Machine (~/.local/share/containers/podman/machine/podman.sock and subdirs).
		machineDir := filepath.Join(homeDir, ".local", "share", "containers", "podman", "machine")
		candidates = append(candidates, socketCandidate{
			provider:    "podman-machine",
			path:        filepath.Join(machineDir, "podman.sock"),
			defaultKind: KindPodman,
		})
		for _, subPath := range discoverPodmanMachineSubdirSockets(machineDir, readDir) {
			candidates = append(candidates, socketCandidate{
				provider:    "podman-machine",
				path:        subPath,
				defaultKind: KindPodman,
			})
		}
	}

	// Podman Machine since Podman 5 on macOS: <machine>-api.sock in
	// $TMPDIR/podman of the shell that started the machine, or in /tmp/podman
	// when it had no TMPDIR. This shell's TMPDIR may differ from that one, or
	// be unset, so the per-user temporary directory is tried as well.
	if goos == "darwin" {
		if userTempDir == nil {
			userTempDir = darwinUserTempDir
		}
		var podmanDirs []string
		for _, tmpDir := range []string{getenv("TMPDIR"), userTempDir(), "/tmp"} {
			tmpDir = strings.TrimSpace(tmpDir)
			if tmpDir == "" || !filepath.IsAbs(tmpDir) {
				continue
			}
			if podmanDir := filepath.Join(tmpDir, "podman"); !slices.Contains(podmanDirs, podmanDir) {
				podmanDirs = append(podmanDirs, podmanDir)
			}
		}
		for _, podmanDir := range podmanDirs {
			for _, apiSocket := range discoverPodmanMachineAPISockets(podmanDir, readDir) {
				candidates = append(candidates, socketCandidate{
					provider:    "podman-machine",
					path:        apiSocket,
					defaultKind: KindPodman,
				})
			}
		}
	}

	if homeDir != "" && filepath.IsAbs(homeDir) {
		// 3. macOS OrbStack (~/.orbstack/run/docker.sock).
		candidates = append(candidates, socketCandidate{
			provider:    "orbstack",
			path:        filepath.Join(homeDir, ".orbstack", "run", "docker.sock"),
			defaultKind: KindDocker,
		})

		// 4. macOS Docker Desktop (~/.docker/run/docker.sock).
		candidates = append(candidates, socketCandidate{
			provider:    "docker-desktop",
			path:        filepath.Join(homeDir, ".docker", "run", "docker.sock"),
			defaultKind: KindDocker,
		})

		// 5. macOS Colima (~/.colima/default/docker.sock).
		candidates = append(candidates, socketCandidate{
			provider:    "colima",
			path:        filepath.Join(homeDir, ".colima", "default", "docker.sock"),
			defaultKind: KindDocker,
		})
	}

	// 6. System Podman and Docker Engine sockets.
	candidates = append(candidates,
		socketCandidate{
			provider:    "podman-system",
			path:        "/run/podman/podman.sock",
			defaultKind: KindPodman,
		},
		socketCandidate{
			provider:    "docker-engine",
			path:        "/var/run/docker.sock",
			defaultKind: KindDocker,
		},
	)

	return candidates
}

func discoverPodmanMachineSubdirSockets(machineDir string, readDir func(string) ([]os.DirEntry, error)) []string {
	entries, err := readDir(machineDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var found []string
	for _, name := range names {
		subDir := filepath.Join(machineDir, name)
		found = append(found, filepath.Join(subDir, "podman.sock"))
		// Podman 5 places provider sockets under machine/<provider>/<vm>/podman.sock
		// (e.g. machine/applehv/podman.sock or machine/libkrun/podman-machine-default/podman.sock).
		nested, nestedErr := readDir(subDir)
		if nestedErr != nil {
			continue
		}
		nestedNames := make([]string, 0, len(nested))
		for _, child := range nested {
			if child.IsDir() && !strings.HasPrefix(child.Name(), ".") {
				nestedNames = append(nestedNames, child.Name())
			}
		}
		sort.Strings(nestedNames)
		for _, childName := range nestedNames {
			found = append(found, filepath.Join(subDir, childName, "podman.sock"))
		}
	}
	return found
}

// darwinUserTempDir returns the macOS per-user temporary directory, or "" when
// getconf cannot tell.
func darwinUserTempDir() string {
	out, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// discoverPodmanMachineAPISockets lists the <machine>-api.sock entries of
// podmanDir in name order. The gvproxy socket beside them is not an API socket.
func discoverPodmanMachineAPISockets(podmanDir string, readDir func(string) ([]os.DirEntry, error)) []string {
	entries, err := readDir(podmanDir)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "-api.sock") {
			found = append(found, filepath.Join(podmanDir, entry.Name()))
		}
	}
	sort.Strings(found)
	return found
}

func parseSocketEndpoint(raw, homeDir string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("socket path is empty")
	}
	if after, ok := strings.CutPrefix(trimmed, "unix://"); ok {
		trimmed = after
	} else if strings.Contains(trimmed, "://") {
		return "", errors.New("only local unix:// container sockets are supported")
	}
	if trimmed == "~" && homeDir != "" {
		trimmed = homeDir
	} else if after, ok := strings.CutPrefix(trimmed, "~/"); ok && homeDir != "" {
		trimmed = filepath.Join(homeDir, after)
	}
	if !filepath.IsAbs(trimmed) {
		return "", errors.New("socket path must be absolute")
	}
	return filepath.Clean(trimmed), nil
}

func inferKindFromPath(socketPath string, fallback RuntimeKind) RuntimeKind {
	lower := strings.ToLower(socketPath)
	switch {
	case strings.Contains(lower, "podman"):
		return KindPodman
	case strings.Contains(lower, "docker") || strings.Contains(lower, "orbstack") || strings.Contains(lower, "colima"):
		return KindDocker
	case fallback != "":
		return fallback
	default:
		return KindDocker
	}
}

func buildDetectedRuntime(kind RuntimeKind, provider, socketPath string) DetectedRuntime {
	if kind == "" {
		kind = inferKindFromPath(socketPath, KindDocker)
	}
	hostAlias := "host.docker.internal"
	if kind == KindPodman {
		hostAlias = "host.containers.internal"
	}
	return DetectedRuntime{
		Kind:       kind,
		Provider:   provider,
		SocketPath: socketPath,
		HostAlias:  hostAlias,
	}
}

func remediationForOS(goos string) string {
	switch goos {
	case "darwin":
		return "start a macOS container runtime (`orb start`, `podman machine start`, `colima start`, or open Docker Desktop), or pass `--socket <unix-socket>`"
	default:
		return "enable a local container socket (`systemctl --user enable --now podman.socket` for rootless Podman, or `sudo systemctl enable --now docker.socket`), or pass `--socket <unix-socket>`"
	}
}

func defaultStatSocket(path string) bool {
	// os.Stat follows symlinks, which is required on macOS where OrbStack,
	// Docker Desktop and Podman Machine expose symlinked socket paths.
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

func defaultPingSocket(ctx context.Context, socketPath string) (RuntimeKind, error) {
	pingCtx, cancel := context.WithTimeout(ctx, defaultSocketPingTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: defaultSocketPingTimeout}
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(dialCtx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: defaultSocketPingTimeout}
	req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, "http://d/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected /_ping HTTP %d", resp.StatusCode)
	}
	if resp.Header.Get("Libpod-Api-Version") != "" ||
		strings.Contains(strings.ToLower(resp.Header.Get("Server")), "libpod") {
		return KindPodman, nil
	}
	return inferKindFromPath(socketPath, KindDocker), nil
}
