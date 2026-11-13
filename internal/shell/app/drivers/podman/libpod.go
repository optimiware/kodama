// Libpod over the Podman API Unix socket.
//
// The client is a thin net/http mapping of the podman.Libpod seam. It
// never imports the containers/podman SDK, never interprets a shell and never
// creates a resource outside the one request it was given. Response bodies are
// bounded, request bodies and environment values never appear in an error and
// transport failures are classified as ErrAmbiguous instead of pretending the
// effect did or did not happen.
package podman

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

const (
	// DefaultAPIVersion is the pinned libpod REST surface this client targets.
	DefaultAPIVersion = "v5.0.0"
	// DefaultMaxResponseBytes bounds one non-log API response.
	DefaultMaxResponseBytes int64 = 8 << 20
	// DefaultRequestTimeout bounds one API call. A caller context can impose a
	// shorter deadline.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultStopTimeoutSeconds is the Podman stop grace period.
	DefaultStopTimeoutSeconds = 10
	// maxLogFrameBytes bounds one multiplexed log frame.
	maxLogFrameBytes = 1 << 20
	// CPU and memory bounds applied to every container this client creates.
	// The ContainerSpec carries no resource fields, so the driver pins
	// the test-environment bound explicitly (1 CPU / 1 GiB).
	DefaultCPUQuota  = 100000
	DefaultCPUPeriod = 100000
	DefaultMemory    = int64(1 << 30)
)

// ErrAmbiguous reports an external effect whose outcome cannot be classified:
// a transport failure, a timeout or a cancellation during the call. The adapter
// above this seam reports an uncertain start/stop, never a fabricated result.
var ErrAmbiguous = errors.New("podman driver: ambiguous external effect")

// LibpodConfig binds one client to one Podman API socket.
type LibpodConfig struct {
	// SocketPath is the Podman API Unix socket. Required and absolute.
	SocketPath string
	// APIVersion overrides the pinned libpod REST version. Empty uses
	// DefaultAPIVersion; a leading "v" is optional.
	APIVersion string
	// Client optionally replaces the HTTP client (tests use an httptest
	// server's transport). Nil builds a Unix-socket client.
	Client *http.Client
	// MaxResponseBytes bounds one response. Zero uses DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// Timeout bounds one call. Zero uses DefaultRequestTimeout; a negative
	// value disables the internal timeout.
	Timeout time.Duration
}

// Libpod is the net/http libpod client. It implements podman.Libpod.
type Libpod struct {
	client       *http.Client
	apiVersion   string
	maxResponse  int64
	requestLimit time.Duration
}

var _ runtimepodman.Libpod = (*Libpod)(nil)

// NewLibpod validates one configuration and builds a client. It opens no
// socket and contacts no daemon.
func NewLibpod(config LibpodConfig) (*Libpod, error) {
	if strings.TrimSpace(config.SocketPath) == "" || !filepath.IsAbs(config.SocketPath) {
		return nil, fault.New(fault.InvalidInput, "podman driver requires an absolute API socket path")
	}
	version := strings.TrimSpace(config.APIVersion)
	if version == "" {
		version = DefaultAPIVersion
	}
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return nil, fault.New(fault.InvalidInput, "podman driver API version is empty")
	}
	maxResponse := config.MaxResponseBytes
	if maxResponse <= 0 {
		maxResponse = DefaultMaxResponseBytes
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	client := config.Client
	if client == nil {
		socketPath := config.SocketPath
		dialer := &net.Dialer{}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		}
		client = &http.Client{Transport: transport}
	}
	return &Libpod{
		client:       client,
		apiVersion:   "v" + version,
		maxResponse:  maxResponse,
		requestLimit: timeout,
	}, nil
}

// request sends one API call and returns the bounded body plus the status. A
// transport failure, timeout, cancellation or oversized response is an error;
// HTTP status handling belongs to each operation.
func (c *Libpod) request(ctx context.Context, method, path string, query url.Values, payload any) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("%w: %s %s cancelled before send: %v", ErrAmbiguous, method, path, err)
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fault.New(fault.InternalFailure, "podman request body is not encodable")
		}
		body = bytes.NewReader(encoded)
	}
	target := "http://d/" + c.apiVersion + "/libpod/" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	requestCtx := ctx
	if c.requestLimit > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, c.requestLimit)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target, body)
	if err != nil {
		return nil, 0, fault.New(fault.InternalFailure, "podman request could not be built")
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s %s: %v", ErrAmbiguous, method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	// Read one byte past the bound so an oversized response is observable
	// instead of silently truncated.
	data, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponse+1))
	if readErr != nil {
		return nil, response.StatusCode, fmt.Errorf("%w: %s %s: reading response: %v", ErrAmbiguous, method, path, readErr)
	}
	if int64(len(data)) > c.maxResponse {
		return nil, response.StatusCode, fault.New(fault.DependencyUnavailable,
			fmt.Sprintf("podman API %s %s response exceeds %d bytes", method, path, c.maxResponse))
	}
	return data, response.StatusCode, nil
}

// Ping probes the Podman API service with one bounded GET of the libpod _ping
// endpoint. It is the readiness check of a composition that runs workloads
// through this client; it creates nothing.
func (c *Libpod) Ping(ctx context.Context) error {
	_, status, err := c.request(ctx, http.MethodGet, "_ping", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError("ping", status)
	}
	return nil
}

// Close releases the client's idle connections. It sends no request, so it
// never stops the external daemon or a workload; it is idempotent.
func (c *Libpod) Close(context.Context) error {
	c.client.CloseIdleConnections()
	return nil
}

// statusError renders a non-secret status failure. The response body is never
// included because a request value could appear in it.
func statusError(operation string, status int) error {
	return fault.New(fault.DependencyUnavailable,
		fmt.Sprintf("podman API %s returned status %d", operation, status))
}

// CreateContainer creates one stable container. It returns ErrAlreadyExists when
// the stable name already exists and ErrNotFound when the image is absent.
func (c *Libpod) CreateContainer(ctx context.Context, spec runtimepodman.ContainerSpec) (runtimepodman.ContainerState, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return runtimepodman.ContainerState{}, fault.New(fault.InvalidInput, "podman create requires a container name")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return runtimepodman.ContainerState{}, fault.New(fault.InvalidInput, "podman create requires an image")
	}
	body, err := createBody(spec)
	if err != nil {
		return runtimepodman.ContainerState{}, err
	}
	data, status, err := c.request(ctx, http.MethodPost, "containers/create", nil, body)
	if err != nil {
		return runtimepodman.ContainerState{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var created struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(data, &created); err != nil {
			return runtimepodman.ContainerState{}, fault.New(fault.DependencyUnavailable,
				"podman create returned an undecodable response")
		}
		return runtimepodman.ContainerState{
			ID:     created.ID,
			Name:   spec.Name,
			Image:  spec.Image,
			Status: runtimepodman.ContainerStatusCreated,
			Labels: copyLabels(spec.Labels),
		}, nil
	case http.StatusNotFound:
		return runtimepodman.ContainerState{}, runtimepodman.ErrNotFound
	case http.StatusConflict:
		return runtimepodman.ContainerState{}, runtimepodman.ErrAlreadyExists
	default:
		return runtimepodman.ContainerState{}, statusError("create", status)
	}
}

// InspectImage checks whether an image reference exists in the local container
// store. An absent image is ErrNotFound.
func (c *Libpod) InspectImage(ctx context.Context, reference string) error {
	if strings.TrimSpace(reference) == "" {
		return fault.New(fault.InvalidInput, "podman image inspect requires an image reference")
	}
	_, status, err := c.request(ctx, http.MethodGet, "images/"+url.PathEscape(reference)+"/exists", nil, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return runtimepodman.ErrNotFound
	default:
		return statusError("image inspect", status)
	}
}

// InspectContainer returns the observed container state. A missing container is
// ErrNotFound, never an absent observation by itself.
func (c *Libpod) InspectContainer(ctx context.Context, name string) (runtimepodman.ContainerState, error) {
	if strings.TrimSpace(name) == "" {
		return runtimepodman.ContainerState{}, fault.New(fault.InvalidInput, "podman inspect requires a container name")
	}
	data, status, err := c.request(ctx, http.MethodGet, "containers/"+url.PathEscape(name)+"/json", nil, nil)
	if err != nil {
		return runtimepodman.ContainerState{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return runtimepodman.ContainerState{}, runtimepodman.ErrNotFound
	default:
		return runtimepodman.ContainerState{}, statusError("inspect", status)
	}
	var inspected inspectContainer
	if err := json.Unmarshal(data, &inspected); err != nil {
		return runtimepodman.ContainerState{}, fault.New(fault.DependencyUnavailable,
			"podman inspect returned an undecodable response")
	}
	return inspected.state(name), nil
}

// ListContainers returns every container matching all supplied labels.
func (c *Libpod) ListContainers(ctx context.Context, labels map[string]string) ([]runtimepodman.ContainerState, error) {
	filters := map[string]map[string]bool{"label": {}}
	for key, value := range labels {
		filters["label"][key+"="+value] = true
	}
	encoded, err := json.Marshal(filters)
	if err != nil {
		return nil, fault.New(fault.InternalFailure, "podman list filter is not encodable")
	}
	query := url.Values{}
	query.Set("all", "true")
	query.Set("filters", string(encoded))
	data, status, err := c.request(ctx, http.MethodGet, "containers/json", query, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("list", status)
	}
	var summaries []containerSummary
	if err := json.Unmarshal(data, &summaries); err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "podman list returned an undecodable response")
	}
	states := make([]runtimepodman.ContainerState, 0, len(summaries))
	for _, summary := range summaries {
		states = append(states, summary.state())
	}
	return states, nil
}

// StartContainer starts one container. An already-started container (304) is
// success; a missing container is ErrNotFound.
func (c *Libpod) StartContainer(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fault.New(fault.InvalidInput, "podman start requires a container name")
	}
	_, status, err := c.request(ctx, http.MethodPost, "containers/"+url.PathEscape(name)+"/start", nil, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return runtimepodman.ErrNotFound
	default:
		return statusError("start", status)
	}
}

// StopContainer requests termination of one container. An already-stopped
// container (304) is success; a missing container is ErrNotFound.
func (c *Libpod) StopContainer(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fault.New(fault.InvalidInput, "podman stop requires a container name")
	}
	query := url.Values{}
	query.Set("t", strconv.Itoa(DefaultStopTimeoutSeconds))
	_, status, err := c.request(ctx, http.MethodPost, "containers/"+url.PathEscape(name)+"/stop", query, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return runtimepodman.ErrNotFound
	default:
		return statusError("stop", status)
	}
}

// RemoveContainer removes one container without touching its named volumes.
//
// Removal is deliberately not forced. The adapter only calls this seam after
// affirmative evidence that the container is created-never-started or has
// whole-container stop evidence, and a non-forced DELETE refuses a container
// that another controller started in the meantime instead of silently killing
// it. A running container must be stopped first; the refusal is surfaced as an
// uncertain removal, never as success.
func (c *Libpod) RemoveContainer(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fault.New(fault.InvalidInput, "podman remove requires a container name")
	}
	query := url.Values{}
	query.Set("volumes", "false")
	_, status, err := c.request(ctx, http.MethodDelete, "containers/"+url.PathEscape(name), query, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return runtimepodman.ErrNotFound
	default:
		return statusError("remove", status)
	}
}

// ReadLogs reads one bounded page of one stream. The API returns a
// multiplexed stream (8-byte frame headers); frames of the requested stream are
// concatenated and sliced at Offset.
func (c *Libpod) ReadLogs(ctx context.Context, request runtimepodman.LogRequest) (runtimepodman.LogPage, error) {
	if strings.TrimSpace(request.Name) == "" {
		return runtimepodman.LogPage{}, fault.New(fault.InvalidInput, "podman log read requires a container name")
	}
	if request.Offset < 0 || request.Limit < 0 {
		return runtimepodman.LogPage{}, fault.New(fault.InvalidInput, "podman log read offset and limit must be non-negative")
	}
	query := url.Values{}
	query.Set("stdout", "true")
	query.Set("stderr", "true")
	query.Set("follow", "false")
	data, status, err := c.request(ctx, http.MethodGet, "containers/"+url.PathEscape(request.Name)+"/logs", query, nil)
	if err != nil {
		return runtimepodman.LogPage{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return runtimepodman.LogPage{}, runtimepodman.ErrNotFound
	default:
		return runtimepodman.LogPage{}, statusError("logs", status)
	}
	wanted := byte(1)
	if request.Stream == "stderr" {
		wanted = 2
	}
	collected, truncated, err := demuxStream(data, wanted, request.Offset+int64(request.Limit)+1)
	if err != nil {
		return runtimepodman.LogPage{}, err
	}
	if request.Offset >= int64(len(collected)) {
		return runtimepodman.LogPage{NextOffset: request.Offset, Truncated: truncated}, nil
	}
	end := min(request.Offset+int64(request.Limit), int64(len(collected)))
	page := append([]byte(nil), collected[request.Offset:end]...)
	if end < int64(len(collected)) {
		truncated = true
	}
	return runtimepodman.LogPage{Data: page, NextOffset: end, Truncated: truncated}, nil
}

// CreateVolume creates one named volume with exactly the requested ownership
// labels. The request carries only the name and the labels, so the volume's
// storage location is the daemon default and cannot be redirected by a driver
// or option field.
//
// A create conflict means a same-named volume already exists. It is adopted
// only when it carries every requested label with the same value, so a retry
// after a lost response is idempotent while a foreign volume with a colliding
// name is refused as Forbidden instead of silently shared. Libpod reports the
// conflict as 409 on some versions and as a 500 whose error cause is "volume
// already exists" on Podman 5.x, so exactly that documented cause is
// reconciled; every other server failure stays a dependency failure. A
// transport failure, timeout or cancellation stays ErrAmbiguous, and an
// undecodable success is a dependency failure, never a fabricated volume.
func (c *Libpod) CreateVolume(ctx context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	if strings.TrimSpace(volume.Name) == "" {
		return runtimepodman.Volume{}, fault.New(fault.InvalidInput, "podman volume create requires a volume name")
	}
	body := volumeCreate{
		Name:   volume.Name,
		Labels: copyLabels(volume.Labels),
	}
	data, status, err := c.request(ctx, http.MethodPost, "volumes/create", nil, body)
	if err != nil {
		return runtimepodman.Volume{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var created volumeCreate
		if err := json.Unmarshal(data, &created); err != nil {
			return runtimepodman.Volume{}, fault.New(fault.DependencyUnavailable,
				"podman volume create returned an undecodable response")
		}
		if created.Name == "" {
			created.Name = volume.Name
		}
		if len(created.Labels) == 0 {
			created.Labels = copyLabels(volume.Labels)
		}
		return runtimepodman.Volume{Name: created.Name, Labels: copyLabels(created.Labels)}, nil
	case http.StatusConflict:
		return c.adoptExistingVolume(ctx, volume)
	default:
		if status >= 500 && volumeAlreadyExists(data) {
			return c.adoptExistingVolume(ctx, volume)
		}
		return runtimepodman.Volume{}, statusError("volume create", status)
	}
}

// adoptExistingVolume reconciles a create conflict by inspecting the existing
// volume. It is adopted only when every requested ownership label is present
// with the same value; any other observation is Forbidden, so a foreign volume
// with a colliding name is never silently shared. An inspect failure keeps its
// own classification instead of becoming a fabricated create result.
func (c *Libpod) adoptExistingVolume(ctx context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	existing, err := c.InspectVolume(ctx, volume.Name)
	if err != nil {
		return runtimepodman.Volume{}, err
	}
	for key, want := range volume.Labels {
		if got, ok := existing.Labels[key]; !ok || got != want {
			return runtimepodman.Volume{}, fault.New(fault.Forbidden,
				"podman volume create found an existing volume with different ownership labels")
		}
	}
	if existing.Name == "" {
		existing.Name = volume.Name
	}
	return existing, nil
}

// volumeAlreadyExists reports whether a server failure body is libpod's
// duplicate-volume conflict. The cause and message are matched, never the whole
// body, and the body is never echoed into an error because it may quote a
// request value.
func volumeAlreadyExists(data []byte) bool {
	var apiError struct {
		Cause   string `json:"cause"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &apiError); err != nil {
		return false
	}
	cause := strings.ToLower(apiError.Cause)
	message := strings.ToLower(apiError.Message)
	return strings.Contains(cause, "volume already exists") ||
		(strings.Contains(message, "volume") && strings.Contains(message, "already exists"))
}

// InspectVolume returns one named volume or ErrNotFound.
func (c *Libpod) InspectVolume(ctx context.Context, name string) (runtimepodman.Volume, error) {
	if strings.TrimSpace(name) == "" {
		return runtimepodman.Volume{}, fault.New(fault.InvalidInput, "podman volume inspect requires a volume name")
	}
	data, status, err := c.request(ctx, http.MethodGet, "volumes/"+url.PathEscape(name)+"/json", nil, nil)
	if err != nil {
		return runtimepodman.Volume{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return runtimepodman.Volume{}, runtimepodman.ErrNotFound
	default:
		return runtimepodman.Volume{}, statusError("volume inspect", status)
	}
	var inspected struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(data, &inspected); err != nil {
		return runtimepodman.Volume{}, fault.New(fault.DependencyUnavailable,
			"podman volume inspect returned an undecodable response")
	}
	return runtimepodman.Volume{Name: inspected.Name, Labels: copyLabels(inspected.Labels)}, nil
}

// RemoveVolume removes one named volume. An absent volume is ErrNotFound, never
// a fabricated success.
//
// Removal is deliberately not forced. Libpod's force delete also removes every
// container still using the volume, which would let one controller silently
// kill another controller's live workload and its data between the adapter's
// reference check and this call. Without force Podman refuses an in-use volume
// with 409; that conflict is surfaced as a non-ErrNotFound,
// non-ErrAlreadyExists uncertain effect so the adapter retains the workspace
// instead of reporting a confirmed release.
func (c *Libpod) RemoveVolume(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fault.New(fault.InvalidInput, "podman volume removal requires a volume name")
	}
	_, status, err := c.request(ctx, http.MethodDelete, "volumes/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return runtimepodman.ErrNotFound
	case http.StatusConflict:
		return fault.New(fault.DependencyUnavailable,
			"podman API volume remove returned status 409: volume is still in use")
	default:
		return statusError("volume remove", status)
	}
}

// ---------------------------------------------------------------------------
// Request and response shapes
// ---------------------------------------------------------------------------

// volumeCreate is the libpod volume-create request and response shape: exactly
// the volume name and its labels, no driver or option field that could change
// where the volume's data lives.
type volumeCreate struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

type createSpec struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Mounts         []bindMount       `json:"mounts,omitempty"`
	Volumes        []volumeMount     `json:"volumes,omitempty"`
	RestartPolicy  string            `json:"restart_policy,omitempty"`
	ResourceLimits *resourceLimits   `json:"resource_limits,omitempty"`
}

type bindMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type volumeMount struct {
	Name        string   `json:"Name"`
	Dest        string   `json:"Dest"`
	Options     []string `json:"Options"`
	IsAnonymous bool     `json:"IsAnonymous"`
	SubPath     string   `json:"SubPath"`
}

type resourceLimits struct {
	CPU    *cpuLimit    `json:"cpu,omitempty"`
	Memory *memoryLimit `json:"memory,omitempty"`
}

type cpuLimit struct {
	Quota  int64 `json:"quota"`
	Period int64 `json:"period"`
}

type memoryLimit struct {
	Limit int64 `json:"limit"`
}

// createBody maps the ContainerSpec onto the libpod create request. A
// bind mount is a `mounts` entry; a named volume is a `volumes` entry. The
// resource bound is applied here because the spec cannot express it.
func createBody(spec runtimepodman.ContainerSpec) (createSpec, error) {
	body := createSpec{
		Name:          spec.Name,
		Image:         spec.Image,
		Entrypoint:    append([]string(nil), spec.Entrypoint...),
		Labels:        copyLabels(spec.Labels),
		RestartPolicy: spec.RestartPolicy,
		ResourceLimits: &resourceLimits{
			CPU:    &cpuLimit{Quota: DefaultCPUQuota, Period: DefaultCPUPeriod},
			Memory: &memoryLimit{Limit: DefaultMemory},
		},
	}
	if len(spec.Env) > 0 {
		body.Env = make(map[string]string, len(spec.Env))
		for _, entry := range spec.Env {
			name, value, found := strings.Cut(entry, "=")
			if !found || name == "" {
				return createSpec{}, fault.New(fault.InvalidInput, "podman environment entry is not NAME=value")
			}
			// Later entries win, so the reserved context appended last can
			// never be shadowed by an admitted literal.
			body.Env[name] = value
		}
	}
	for _, mount := range spec.Mounts {
		switch mount.Type {
		case "bind":
			entry := bindMount{Destination: mount.Destination, Type: "bind", Source: mount.Source}
			if mount.ReadOnly {
				entry.Options = []string{"ro"}
			}
			body.Mounts = append(body.Mounts, entry)
		case "volume":
			body.Volumes = append(body.Volumes, volumeMount{
				Name:        mount.Source,
				Dest:        mount.Destination,
				Options:     nil,
				IsAnonymous: false,
				SubPath:     "",
			})
		default:
			return createSpec{}, fault.New(fault.InvalidInput, "podman mount type is not bind or volume")
		}
	}
	return body, nil
}

type inspectContainer struct {
	ID        string `json:"Id"`
	Name      string `json:"Name"`
	Image     string `json:"Image"`
	ImageName string `json:"ImageName"`
	State     struct {
		Status     string    `json:"Status"`
		Running    bool      `json:"Running"`
		Pid        int       `json:"Pid"`
		ExitCode   int       `json:"ExitCode"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (inspected inspectContainer) state(fallbackName string) runtimepodman.ContainerState {
	name := inspected.Name
	if name == "" {
		name = fallbackName
	}
	image := inspected.ImageName
	if image == "" {
		image = inspected.Image
	}
	status := mapStatus(inspected.State.Status)
	started := inspected.State.Running || !inspected.State.StartedAt.IsZero()
	wholeStopped := status == runtimepodman.ContainerStatusExited &&
		!inspected.State.Running && inspected.State.Pid == 0 && !inspected.State.FinishedAt.IsZero()
	return runtimepodman.ContainerState{
		ID:           inspected.ID,
		Name:         name,
		Image:        image,
		Status:       status,
		Started:      started,
		ExitCode:     inspected.State.ExitCode,
		StartPending: false,
		WholeStopped: wholeStopped,
		Labels:       copyLabels(inspected.Config.Labels),
	}
}

type containerSummary struct {
	ID       string            `json:"Id"`
	Names    []string          `json:"Names"`
	Image    string            `json:"Image"`
	ImageID  string            `json:"ImageID"`
	State    string            `json:"State"`
	Labels   map[string]string `json:"Labels"`
	Exited   bool              `json:"Exited"`
	ExitCode int               `json:"ExitCode"`
	Pid      int               `json:"Pid"`
}

func (summary containerSummary) state() runtimepodman.ContainerState {
	name := ""
	if len(summary.Names) > 0 {
		name = summary.Names[0]
	}
	status := mapStatus(summary.State)
	wholeStopped := summary.Exited && summary.Pid == 0
	return runtimepodman.ContainerState{
		ID:           summary.ID,
		Name:         name,
		Image:        summary.Image,
		Status:       status,
		Started:      status == runtimepodman.ContainerStatusRunning || summary.Exited,
		ExitCode:     summary.ExitCode,
		WholeStopped: wholeStopped,
		Labels:       copyLabels(summary.Labels),
	}
}

// mapStatus maps a libpod status onto the seam vocabulary. Any status
// the seam does not define is returned verbatim so the adapter reports unknown
// instead of guessing.
func mapStatus(status string) string {
	switch status {
	case "created":
		return runtimepodman.ContainerStatusCreated
	case "running":
		return runtimepodman.ContainerStatusRunning
	case "exited", "stopped":
		return runtimepodman.ContainerStatusExited
	default:
		return status
	}
}

// demuxStream concatenates the frames of one multiplexed stream until stop
// bytes have been collected or the bounded scan is exhausted. It reports
// truncation when more of the requested stream remains.
func demuxStream(data []byte, wanted byte, stop int64) ([]byte, bool, error) {
	collected := make([]byte, 0, 256)
	truncated := false
	position := 0
	for position < len(data) {
		if position+8 > len(data) {
			// A truncated frame is not a fabricated chunk.
			truncated = true
			break
		}
		frameStream := data[position]
		length := int(binary.BigEndian.Uint32(data[position+4 : position+8]))
		position += 8
		if length < 0 || length > maxLogFrameBytes || position+length > len(data) {
			return nil, false, fault.New(fault.DependencyUnavailable, "podman log frame is malformed")
		}
		frame := data[position : position+length]
		position += length
		if frameStream != wanted {
			continue
		}
		collected = append(collected, frame...)
		if int64(len(collected)) >= stop {
			// More frames may remain; the page is already full.
			truncated = position < len(data) || int64(len(collected)) > stop
			break
		}
	}
	return collected, truncated, nil
}

func copyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	copied := make(map[string]string, len(labels))
	maps.Copy(copied, labels)
	return copied
}
