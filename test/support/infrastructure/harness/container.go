package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Port declares a container port published on loopback with a
// Podman-allocated host port.
type Port struct {
	Container int
	Protocol  string // tcp (default), udp, sctp
}

// Volume declares an ephemeral volume. An empty Name mounts an anonymous
// volume (removed with the container); a set Name mounts a named volume that
// the shard tracks and removes.
type Volume struct {
	Name   string
	Target string
}

// RunSpec describes one container. The shard fills Name, Network and the
// reserved labels when they are empty.
type RunSpec struct {
	Image   string
	Name    string
	Network string
	Ports   []Port
	Volumes []Volume
	Env     map[string]string
	Labels  map[string]string
	Cmd     []string
	CPUs    string
	Memory  string
}

// runArgs builds the podman run argument vector. It is pure so unit tests can
// assert exact construction, including that no shell is involved.
func runArgs(spec RunSpec, shardID string, ownerPID int) ([]string, error) {
	if strings.TrimSpace(spec.Image) == "" {
		return nil, errors.New("harness: run spec needs an image")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("harness: run spec needs a container name")
	}
	labels := map[string]string{}
	maps.Copy(labels, spec.Labels)
	// Reserved labels always win over caller labels.
	labels[LabelTest] = "1"
	labels[LabelShard] = shardID
	labels[LabelOwnerPID] = strconv.Itoa(ownerPID)

	args := []string{
		"run",
		"--detach",
		"--name", spec.Name,
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	cpus := spec.CPUs
	if cpus == "" {
		cpus = DefaultCPUs
	}
	memory := spec.Memory
	if memory == "" {
		memory = DefaultMemory
	}
	args = append(args, "--cpus", cpus, "--memory", memory)

	for _, port := range spec.Ports {
		if port.Container <= 0 || port.Container > 65535 {
			return nil, fmt.Errorf("harness: invalid container port %d", port.Container)
		}
		protocol := strings.ToLower(port.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		switch protocol {
		case "tcp", "udp", "sctp":
		default:
			return nil, fmt.Errorf("harness: invalid port protocol %q", port.Protocol)
		}
		publish := "127.0.0.1::" + strconv.Itoa(port.Container)
		if protocol != "tcp" {
			publish += "/" + protocol
		}
		args = append(args, "-p", publish)
	}
	for _, volume := range spec.Volumes {
		if strings.TrimSpace(volume.Target) == "" {
			return nil, fmt.Errorf("harness: volume needs a target path")
		}
		mount := volume.Target
		if volume.Name != "" {
			mount = volume.Name + ":" + volume.Target
		}
		args = append(args, "-v", mount)
	}
	envKeys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		args = append(args, "--env", key+"="+spec.Env[key])
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)
	return args, nil
}

// Container is one running container owned by a shard.
type Container struct {
	Name    string
	ShardID string

	podman *Podman

	mu    sync.Mutex
	ports map[int]int
}

// HostPort looks up the Podman-allocated host port for a published container
// TCP port.
func (c *Container) HostPort(ctx context.Context, containerPort int) (int, error) {
	return c.HostPortProto(ctx, containerPort, "tcp")
}

// HostPortProto looks up the allocated host port for a container port and
// protocol.
func (c *Container) HostPortProto(ctx context.Context, containerPort int, protocol string) (int, error) {
	if protocol == "" {
		protocol = "tcp"
	}
	protocol = strings.ToLower(protocol)
	cacheKey := containerPort
	if protocol != "tcp" {
		// Cache only tcp ports in the int map; other protocols are rare and
		// always re-resolved.
		cacheKey = -containerPort
	}
	c.mu.Lock()
	if port, ok := c.ports[cacheKey]; ok {
		c.mu.Unlock()
		return port, nil
	}
	c.mu.Unlock()

	res, err := c.podman.run(ctx, c.podman.operationLimit, "port", c.Name,
		strconv.Itoa(containerPort)+"/"+protocol)
	if err != nil {
		return 0, fmt.Errorf("harness: look up port %d/%s on %s: %w", containerPort, protocol, c.Name, err)
	}
	hostPort, err := parsePortOutput(string(res.Stdout))
	if err != nil {
		return 0, fmt.Errorf("harness: look up port %d/%s on %s: %w", containerPort, protocol, c.Name, err)
	}
	c.mu.Lock()
	c.ports[cacheKey] = hostPort
	c.mu.Unlock()
	return hostPort, nil
}

// Addr returns the loopback address for a published container port.
func (c *Container) Addr(ctx context.Context, containerPort int) (string, error) {
	hostPort, err := c.HostPort(ctx, containerPort)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), nil
}

func parsePortOutput(output string) (int, error) {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, portText, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no published port in %q", strings.TrimSpace(output))
}

// Logs captures the container log output (stdout and stderr).
func (c *Container) Logs(ctx context.Context) ([]byte, error) {
	res, err := c.podman.run(ctx, c.podman.operationLimit, "logs", c.Name)
	combined := append(append([]byte(nil), res.Stdout...), res.Stderr...)
	if err != nil {
		return combined, fmt.Errorf("harness: logs %s: %w", c.Name, err)
	}
	return combined, nil
}

// Stop stops the container with a bounded grace period. A container that is
// already gone is not an error, so cleanup stays idempotent.
func (c *Container) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.podman.stopLimit)
	defer cancel()
	grace := max(int(c.podman.stopGrace.Seconds()), 0)
	if _, err := c.podman.run(ctx, 0, "stop", "--time", strconv.Itoa(grace), c.Name); err != nil {
		if missingContainer(err) {
			return nil
		}
		return fmt.Errorf("harness: stop %s: %w", c.Name, err)
	}
	return nil
}

// Remove force-removes the container and its anonymous volumes. A container
// that is already gone is not an error.
func (c *Container) Remove(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.podman.stopLimit)
	defer cancel()
	if _, err := c.podman.run(ctx, 0, "rm", "--force", "--volumes", c.Name); err != nil {
		if missingContainer(err) {
			return nil
		}
		return fmt.Errorf("harness: remove %s: %w", c.Name, err)
	}
	return nil
}

func missingContainer(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such container") ||
		strings.Contains(text, "no container with") ||
		strings.Contains(text, "does not exist")
}

// Cleanup stops then removes the container, trying both steps even when the
// first fails. Bounded by the stop timeout for each step.
func (c *Container) Cleanup(ctx context.Context) error {
	stopErr := c.Stop(ctx)
	removeErr := c.Remove(ctx)
	return errors.Join(stopErr, removeErr)
}
