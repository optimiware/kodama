package up

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	dockerAPIVersion         = "v1.41"
	defaultEngineHTTPTimeout = 10 * time.Minute
	maxEngineResponseBytes   = 8 << 20
)

// EngineClient talks to a Podman or Docker Engine Unix socket over the shared
// Docker-compatible HTTP API (/v1.41/*) so stack operations work identically on
// macOS (OrbStack, Docker Desktop, Colima, Podman Machine) and Linux (Podman,
// Docker Engine) without requiring matching host CLIs or putting secrets in argv.
type EngineClient struct {
	socketPath string
	httpClient *http.Client
}

// PortPublish binds one container TCP port to a host loopback port (hostPort 0
// allocates an ephemeral loopback port).
type PortPublish struct {
	ContainerPort int
	HostIP        string
	HostPort      int
}

// MountBind describes a bind or named-volume mount.
type MountBind struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ContainerSpec describes one detached or one-shot container.
type ContainerSpec struct {
	Name       string
	Image      string
	Cmd        []string
	Entrypoint []string
	Env        []string
	Labels     map[string]string
	Network    string
	Mounts     []MountBind
	Ports      []PortPublish
	User       string
}

// ContainerInfo summarizes a container's inspected runtime state.
type ContainerInfo struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Exists    bool              `json:"exists"`
	Running   bool              `json:"running"`
	Status    string            `json:"status"`
	ExitCode  int               `json:"exit_code"`
	Labels    map[string]string `json:"labels"`
	HostPorts map[string]int    `json:"host_ports"`
}

// NewEngineClient constructs a Unix-socket Engine API client. An optional
// custom *http.Client may be supplied by tests.
func NewEngineClient(socketPath string, custom *http.Client) (*EngineClient, error) {
	trimmed := strings.TrimSpace(socketPath)
	if trimmed == "" {
		return nil, errors.New("engine socket path is empty")
	}
	client := custom
	if client == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", trimmed)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		}
		client = &http.Client{
			Transport: transport,
			Timeout:   defaultEngineHTTPTimeout,
		}
	}
	return &EngineClient{socketPath: trimmed, httpClient: client}, nil
}

// EnsureNetwork creates a labelled bridge network if it does not already exist.
func (c *EngineClient) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	status, _, err := c.doJSON(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	payload := map[string]any{
		"Name":   name,
		"Driver": "bridge",
		"Labels": labels,
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, "/networks/create", payload)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("create network %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
	}
	return nil
}

// RemoveNetwork deletes a network if present.
func (c *EngineClient) RemoveNetwork(ctx context.Context, name string) error {
	status, body, err := c.doJSON(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("remove network %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
}

// EnsureVolume creates a labelled named volume or verifies an existing volume's
// ownership label.
func (c *EngineClient) EnsureVolume(ctx context.Context, name, instance string, labels map[string]string) error {
	status, body, err := c.doJSON(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		var existing struct {
			Labels map[string]string `json:"Labels"`
		}
		if err := json.Unmarshal(body, &existing); err == nil {
			if owner := existing.Labels[LabelInstance]; owner != instance {
				return fmt.Errorf("refusing existing volume %q: not owned by kodama instance %q", name, instance)
			}
		}
		return nil
	}
	payload := map[string]any{
		"Name":   name,
		"Labels": labels,
	}
	status, body, err = c.doJSON(ctx, http.MethodPost, "/volumes/create", payload)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("create volume %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
	}
	return nil
}

// RemoveVolume removes a named volume if present.
func (c *EngineClient) RemoveVolume(ctx context.Context, name string) error {
	status, body, err := c.doJSON(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name)+"?force=true", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("remove volume %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
}

// EnsureImage checks whether imageRef exists locally and pulls it if missing.
func (c *EngineClient) EnsureImage(ctx context.Context, imageRef string) error {
	status, _, err := c.doJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(imageRef)+"/json", nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	q := url.Values{"fromImage": {imageRef}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://d/"+dockerAPIVersion+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", imageRef, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pull image %q failed (HTTP %d)", imageRef, resp.StatusCode)
	}
	return nil
}

// InspectContainer returns a container's current state and published host ports.
func (c *EngineClient) InspectContainer(ctx context.Context, name string) (ContainerInfo, error) {
	status, body, err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return ContainerInfo{}, err
	}
	if status == http.StatusNotFound {
		return ContainerInfo{Name: name, Exists: false, Status: "absent"}, nil
	}
	if status != http.StatusOK {
		return ContainerInfo{}, fmt.Errorf("inspect container %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
	}
	var raw struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Status   string `json:"Status"`
			Running  bool   `json:"Running"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ContainerInfo{}, fmt.Errorf("decode inspect container %q: %w", name, err)
	}
	hostPorts := make(map[string]int)
	for protoKey, bindings := range raw.NetworkSettings.Ports {
		for _, b := range bindings {
			if p, convErr := strconv.Atoi(b.HostPort); convErr == nil && p > 0 {
				hostPorts[protoKey] = p
				break
			}
		}
	}
	return ContainerInfo{
		ID:        raw.ID,
		Name:      strings.TrimPrefix(raw.Name, "/"),
		Exists:    true,
		Running:   raw.State.Running,
		Status:    raw.State.Status,
		ExitCode:  raw.State.ExitCode,
		Labels:    raw.Config.Labels,
		HostPorts: hostPorts,
	}, nil
}

// ListInstanceContainers returns containers carrying kodama.up.instance=<instance>.
func (c *EngineClient) ListInstanceContainers(ctx context.Context, instance string) ([]ContainerInfo, error) {
	filterBytes, err := json.Marshal(map[string][]string{
		"label": {LabelInstance + "=" + instance},
	})
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"all":     {"true"},
		"filters": {string(filterBytes)},
	}
	status, body, err := c.doJSON(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list containers failed (HTTP %d): %s", status, engineErrMsg(body))
	}
	var items []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(items))
	for _, item := range items {
		name := item.ID
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		out = append(out, ContainerInfo{
			ID:      item.ID,
			Name:    name,
			Exists:  true,
			Running: item.State == "running",
			Status:  item.State,
			Labels:  item.Labels,
		})
	}
	return out, nil
}

// RemoveContainer force-removes a container if present.
func (c *EngineClient) RemoveContainer(ctx context.Context, name string) error {
	status, body, err := c.doJSON(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=true&v=false", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("remove container %q failed (HTTP %d): %s", name, status, engineErrMsg(body))
}

// StartDetached creates and starts spec, returning the inspected running state.
func (c *EngineClient) StartDetached(ctx context.Context, spec ContainerSpec) (ContainerInfo, error) {
	id, err := c.createContainer(ctx, spec)
	if err != nil {
		return ContainerInfo{}, err
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
	if err != nil {
		_ = c.RemoveContainer(context.Background(), id)
		return ContainerInfo{}, err
	}
	if status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotModified {
		_ = c.RemoveContainer(context.Background(), id)
		return ContainerInfo{}, fmt.Errorf("start container %q failed (HTTP %d): %s", spec.Name, status, engineErrMsg(body))
	}
	return c.InspectContainer(ctx, id)
}

// RunOneShot creates, starts, waits for and removes a short-lived container.
func (c *EngineClient) RunOneShot(ctx context.Context, spec ContainerSpec) (string, error) {
	id, err := c.createContainer(ctx, spec)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = c.RemoveContainer(context.Background(), id)
	}()

	status, body, err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return "", fmt.Errorf("start one-shot container %q failed (HTTP %d): %s", spec.Name, status, engineErrMsg(body))
	}

	status, waitBody, err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("wait container %q failed (HTTP %d): %s", spec.Name, status, engineErrMsg(waitBody))
	}
	var waitResp struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := json.Unmarshal(waitBody, &waitResp); err != nil {
		return "", err
	}
	logs, _ := c.containerLogs(ctx, id)
	if waitResp.StatusCode != 0 {
		return logs, fmt.Errorf("container %q exited with status %d", spec.Name, waitResp.StatusCode)
	}
	return logs, nil
}

// Exec runs cmd inside an existing running container and returns its exit code.
func (c *EngineClient) Exec(ctx context.Context, containerName string, cmd []string) (int, error) {
	createPayload := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(containerName)+"/exec", createPayload)
	if err != nil {
		return -1, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return -1, fmt.Errorf("exec create in %q failed (HTTP %d): %s", containerName, status, engineErrMsg(body))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return -1, errors.New("exec create returned no id")
	}
	startPayload := map[string]any{
		"Detach": false,
		"Tty":    false,
	}
	status, _, err = c.doJSON(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", startPayload)
	if err != nil {
		return -1, err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return -1, fmt.Errorf("exec start failed (HTTP %d)", status)
	}
	status, inspectBody, err := c.doJSON(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil)
	if err != nil || status != http.StatusOK {
		return -1, errors.New("exec inspect failed")
	}
	var inspected struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := json.Unmarshal(inspectBody, &inspected); err != nil {
		return -1, err
	}
	return inspected.ExitCode, nil
}

func (c *EngineClient) createContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	exposed := make(map[string]struct{}, len(spec.Ports))
	portBindings := make(map[string][]map[string]string, len(spec.Ports))
	for _, p := range spec.Ports {
		key := fmt.Sprintf("%d/tcp", p.ContainerPort)
		exposed[key] = struct{}{}
		hostIP := p.HostIP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
		hostPort := "0"
		if p.HostPort > 0 {
			hostPort = strconv.Itoa(p.HostPort)
		}
		portBindings[key] = append(portBindings[key], map[string]string{
			"HostIp":   hostIP,
			"HostPort": hostPort,
		})
	}

	binds := make([]string, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		entry := m.Source + ":" + m.Target
		if m.ReadOnly {
			entry += ":ro"
		}
		binds = append(binds, entry)
	}

	hostCfg := map[string]any{
		"Binds":        binds,
		"PortBindings": portBindings,
		"ExtraHosts": []string{
			"host.containers.internal:host-gateway",
			"host.docker.internal:host-gateway",
		},
	}
	if spec.Network != "" {
		hostCfg["NetworkMode"] = spec.Network
	}

	payload := map[string]any{
		"Image":        spec.Image,
		"Env":          spec.Env,
		"Labels":       spec.Labels,
		"ExposedPorts": exposed,
		"HostConfig":   hostCfg,
	}
	if len(spec.Cmd) > 0 {
		payload["Cmd"] = spec.Cmd
	}
	if len(spec.Entrypoint) > 0 {
		payload["Entrypoint"] = spec.Entrypoint
	}
	if spec.User != "" {
		payload["User"] = spec.User
	}

	path := "/containers/create"
	if spec.Name != "" {
		path += "?name=" + url.QueryEscape(spec.Name)
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("create container %q failed (HTTP %d): %s", spec.Name, status, engineErrMsg(body))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("create container %q returned no id", spec.Name)
	}
	return created.ID, nil
}

func (c *EngineClient) containerLogs(ctx context.Context, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://d/"+dockerAPIVersion+"/containers/"+url.PathEscape(id)+"/logs?stdout=true&stderr=true", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	// Strip Docker multiplexed 8-byte frame headers when present; keep printable UTF-8.
	return stripLogFrames(raw), nil
}

func stripLogFrames(raw []byte) string {
	var out bytes.Buffer
	rest := raw
	for len(rest) >= 8 && (rest[0] == 1 || rest[0] == 2) && rest[1] == 0 && rest[2] == 0 && rest[3] == 0 {
		size := int(rest[4])<<24 | int(rest[5])<<16 | int(rest[6])<<8 | int(rest[7])
		rest = rest[8:]
		if size < 0 || size > len(rest) {
			out.Write(rest)
			return out.String()
		}
		out.Write(rest[:size])
		rest = rest[size:]
	}
	if out.Len() == 0 {
		return string(raw)
	}
	out.Write(rest)
	return out.String()
}

func (c *EngineClient) doJSON(ctx context.Context, method, relPath string, payload any) (int, []byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://d/"+dockerAPIVersion+relPath, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEngineResponseBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func engineErrMsg(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &parsed); err == nil && parsed.Message != "" {
		return parsed.Message
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 256 {
		trimmed = trimmed[:256]
	}
	if trimmed == "" {
		return "unknown engine error"
	}
	return trimmed
}
