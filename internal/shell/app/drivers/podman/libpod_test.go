package podman

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	runtimepodman "github.com/optimiware/kodema/internal/shell/runtime/podman"
)

// unixServer starts an httptest server on a private Unix socket and returns the
// socket path. It is the only "Podman daemon" these unit tests touch.
func unixServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "podman.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)
	return socket
}

func newTestClient(t *testing.T, handler http.Handler) *Libpod {
	t.Helper()
	client, err := NewLibpod(LibpodConfig{SocketPath: unixServer(t, handler), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewLibpod: %v", err)
	}
	return client
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestCreateContainerMapsSpecAndReturnsIdentity(t *testing.T) {
	var got createSpec
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v5.0.0/libpod/containers/create" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "container-1"})
	}))
	state, err := client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{
		Name:       "kodema-workload-1",
		Image:      "registry.example.com/tools/build@sha256:" + strings.Repeat("a", 64),
		Entrypoint: []string{"/bin/sh", "/kodema/script"},
		Env:        []string{"BUILD_MODE=release", "KODEMA_URL=https://kodema.example.com"},
		Labels:     map[string]string{"kodema.dev/workload": "workload-1"},
		Mounts: []runtimepodman.Mount{
			{Type: "bind", Source: "/host/kodema", Destination: "/kodema", ReadOnly: true},
			{Type: "volume", Source: "kodema-workspace-1", Destination: "/workspace"},
		},
		RestartPolicy: "no",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if state.ID != "container-1" || state.Name != "kodema-workload-1" || state.Status != runtimepodman.ContainerStatusCreated {
		t.Fatalf("created state = %+v", state)
	}
	if got.Name != "kodema-workload-1" || got.RestartPolicy != "no" {
		t.Fatalf("create body name/restart = %q/%q", got.Name, got.RestartPolicy)
	}
	if len(got.Entrypoint) != 2 || got.Entrypoint[1] != "/kodema/script" {
		t.Fatalf("create body entrypoint = %v", got.Entrypoint)
	}
	if got.Env["BUILD_MODE"] != "release" || got.Env["KODEMA_URL"] != "https://kodema.example.com" {
		t.Fatalf("create body env = %v", got.Env)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Type != "bind" || !contains(got.Mounts[0].Options, "ro") {
		t.Fatalf("create body bind mounts = %+v", got.Mounts)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Name != "kodema-workspace-1" || got.Volumes[0].Dest != "/workspace" {
		t.Fatalf("create body volumes = %+v", got.Volumes)
	}
	if got.ResourceLimits == nil || got.ResourceLimits.CPU == nil || got.ResourceLimits.Memory == nil {
		t.Fatalf("create body has no bounded resource limits: %+v", got.ResourceLimits)
	}
	if got.ResourceLimits.CPU.Quota != DefaultCPUQuota || got.ResourceLimits.Memory.Limit != DefaultMemory {
		t.Fatalf("create body resource limits = %+v", got.ResourceLimits)
	}
}

// TestCreateContainerMapsTheToolboxMountsAndWorkingDirectory pins the create
// body of a workload with a toolbox: the working directory, the nested
// read-only toolbox bind and the bounded, writable output tmpfs.
func TestCreateContainerMapsTheToolboxMountsAndWorkingDirectory(t *testing.T) {
	var got createSpec
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "container-1"})
	}))
	if _, err := client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{
		Name:       "kodema-workload-1",
		Image:      "registry.example.com/tools/build@sha256:" + strings.Repeat("a", 64),
		Entrypoint: []string{"/bin/sh", "-c", "launcher"},
		WorkingDir: "/workspace",
		Mounts: []runtimepodman.Mount{
			{Type: "bind", Source: "/host/kodema", Destination: "/kodema", ReadOnly: true},
			{Type: "volume", Source: "kodema-workspace-1", Destination: "/workspace"},
			{Type: "bind", Source: "/host/.toolbox/abc", Destination: "/kodema/bin", ReadOnly: true},
			{Type: "tmpfs", Destination: "/kodema/outputs/build", SizeBytes: 1 << 20},
		},
		RestartPolicy: "no",
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got.WorkDir != "/workspace" {
		t.Fatalf("create body work_dir = %q; want /workspace", got.WorkDir)
	}
	if len(got.Mounts) != 3 {
		t.Fatalf("create body mounts = %+v; want the two binds and the tmpfs", got.Mounts)
	}
	toolbox, outputs := got.Mounts[1], got.Mounts[2]
	if toolbox.Type != "bind" || toolbox.Destination != "/kodema/bin" || toolbox.Source != "/host/.toolbox/abc" || !contains(toolbox.Options, "ro") {
		t.Fatalf("toolbox mount = %+v; want a read-only bind", toolbox)
	}
	if outputs.Type != "tmpfs" || outputs.Destination != "/kodema/outputs/build" {
		t.Fatalf("output mount = %+v; want a tmpfs", outputs)
	}
	for _, option := range []string{"rw", "noexec", "nosuid", "nodev", "mode=1777", "size=1048576"} {
		if !contains(outputs.Options, option) {
			t.Fatalf("output mount options = %v; want %q", outputs.Options, option)
		}
	}
	if contains(outputs.Options, "ro") {
		t.Fatalf("output mount options = %v; the directory must be writable", outputs.Options)
	}
}

func TestCreateContainerMapsNotFoundAndConflict(t *testing.T) {
	status := http.StatusNotFound
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	_, err := client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{Name: "c", Image: "i"})
	if !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("create 404 error = %v; want ErrNotFound", err)
	}
	status = http.StatusConflict
	_, err = client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{Name: "c", Image: "i"})
	if !errors.Is(err, runtimepodman.ErrAlreadyExists) {
		t.Fatalf("create 409 error = %v; want ErrAlreadyExists", err)
	}
}

func TestInspectContainerMapsState(t *testing.T) {
	var body string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	cases := []struct {
		name      string
		json      string
		status    string
		started   bool
		exited    bool
		whole     bool
		exitCode  int
		wantImage string
	}{
		{
			name:   "created",
			json:   `{"Id":"1","Name":"c","ImageName":"img@sha256:x","State":{"Status":"created"},"Config":{"Labels":{"a":"b"}}}`,
			status: runtimepodman.ContainerStatusCreated,
		},
		{
			name:    "running",
			json:    `{"Id":"1","Name":"c","ImageName":"img@sha256:x","State":{"Status":"running","Running":true,"Pid":42,"StartedAt":"2026-01-01T00:00:00Z"},"Config":{"Labels":{"a":"b"}}}`,
			status:  runtimepodman.ContainerStatusRunning,
			started: true,
		},
		{
			name:     "exited",
			json:     `{"Id":"1","Name":"c","ImageName":"img@sha256:x","State":{"Status":"exited","Running":false,"Pid":0,"ExitCode":7,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-01T00:01:00Z"},"Config":{"Labels":{"a":"b"}}}`,
			status:   runtimepodman.ContainerStatusExited,
			started:  true,
			exited:   true,
			whole:    true,
			exitCode: 7,
		},
		{
			name:    "exited without finish evidence",
			json:    `{"Id":"1","Name":"c","ImageName":"img@sha256:x","State":{"Status":"exited","Running":false,"Pid":0,"ExitCode":7,"StartedAt":"2026-01-01T00:00:00Z"},"Config":{"Labels":{"a":"b"}}}`,
			status:  runtimepodman.ContainerStatusExited,
			started: true,
		},
	}
	for _, tc := range cases {
		body = tc.json
		state, err := client.InspectContainer(context.Background(), "c")
		if err != nil {
			t.Fatalf("%s: InspectContainer: %v", tc.name, err)
		}
		if state.Status != tc.status || state.Started != tc.started || state.WholeStopped != tc.whole {
			t.Fatalf("%s: state = %+v", tc.name, state)
		}
		if tc.exited && state.ExitCode != tc.exitCode {
			t.Fatalf("%s: exit code = %d; want %d", tc.name, state.ExitCode, tc.exitCode)
		}
		if state.Labels["a"] != "b" {
			t.Fatalf("%s: labels = %v", tc.name, state.Labels)
		}
	}
}

func TestInspectContainerNotFound(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err := client.InspectContainer(context.Background(), "missing"); !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("inspect error = %v; want ErrNotFound", err)
	}
}

func TestStartStopRemoveMapStatuses(t *testing.T) {
	status := http.StatusNoContent
	var removeQuery url.Values
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/v5.0.0/libpod/containers/c" {
			removeQuery = r.URL.Query()
		}
		w.WriteHeader(status)
	}))
	if err := client.StartContainer(context.Background(), "c"); err != nil {
		t.Fatalf("start 204: %v", err)
	}
	status = http.StatusNotModified
	if err := client.StartContainer(context.Background(), "c"); err != nil {
		t.Fatalf("start 304: %v", err)
	}
	status = http.StatusNoContent
	if err := client.StopContainer(context.Background(), "c"); err != nil {
		t.Fatalf("stop 204: %v", err)
	}
	status = http.StatusNotModified
	if err := client.StopContainer(context.Background(), "c"); err != nil {
		t.Fatalf("stop 304: %v", err)
	}
	status = http.StatusNotFound
	if err := client.RemoveContainer(context.Background(), "c"); !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("remove 404 error = %v; want ErrNotFound", err)
	}
	status = http.StatusOK
	if err := client.RemoveContainer(context.Background(), "c"); err != nil {
		t.Fatalf("remove 200: %v", err)
	}
	// Removal must not force: a container another controller started between
	// the adapter's check and this call is refused by Podman, not killed.
	if removeQuery == nil {
		t.Fatal("container DELETE was not observed")
	}
	if removeQuery.Has("force") {
		t.Fatalf("container DELETE query = %q; want no force parameter", removeQuery.Encode())
	}
	if got := removeQuery.Get("volumes"); got != "false" {
		t.Fatalf("container DELETE volumes = %q; want false", got)
	}
}

func TestListContainersEncodesLabelFilters(t *testing.T) {
	var gotFilters string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5.0.0/libpod/containers/json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotFilters = r.URL.Query().Get("filters")
		writeJSON(w, http.StatusOK, []map[string]any{{
			"Id":     "1",
			"Names":  []string{"kodema-workload-1"},
			"Image":  "img",
			"State":  "exited",
			"Exited": true,
			"Pid":    0,
			"Labels": map[string]string{"kodema.dev/workspace": "workspace-1"},
		}})
	}))
	containers, err := client.ListContainers(context.Background(), map[string]string{
		"kodema.dev/workspace": "workspace-1",
	})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "kodema-workload-1" || !containers[0].WholeStopped {
		t.Fatalf("containers = %+v", containers)
	}
	var decoded map[string]map[string]bool
	if err := json.Unmarshal([]byte(gotFilters), &decoded); err != nil {
		t.Fatalf("filters %q: %v", gotFilters, err)
	}
	if !decoded["label"]["kodema.dev/workspace=workspace-1"] {
		t.Fatalf("filters = %v", decoded)
	}
}

func TestReadLogsDemuxesAndPaginates(t *testing.T) {
	frame := func(stream byte, text string) []byte {
		out := make([]byte, 8+len(text))
		out[0] = stream
		binary.BigEndian.PutUint32(out[4:8], uint32(len(text)))
		copy(out[8:], text)
		return out
	}
	payload := append(append(frame(1, "hello "), frame(2, "oops")...), frame(1, "world")...)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stdout") != "true" || r.URL.Query().Get("stderr") != "true" {
			t.Errorf("log query = %v", r.URL.Query())
		}
		_, _ = w.Write(payload)
	}))
	page, err := client.ReadLogs(context.Background(), runtimepodman.LogRequest{
		Name: "c", Stream: "stdout", Offset: 0, Limit: 6,
	})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if string(page.Data) != "hello " || !page.Truncated {
		t.Fatalf("first page = %q truncated=%v", page.Data, page.Truncated)
	}
	page, err = client.ReadLogs(context.Background(), runtimepodman.LogRequest{
		Name: "c", Stream: "stdout", Offset: 6, Limit: 6,
	})
	if err != nil {
		t.Fatalf("ReadLogs second: %v", err)
	}
	if string(page.Data) != "world" || page.Truncated {
		t.Fatalf("second page = %q truncated=%v", page.Data, page.Truncated)
	}
	page, err = client.ReadLogs(context.Background(), runtimepodman.LogRequest{
		Name: "c", Stream: "stderr", Offset: 0, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ReadLogs stderr: %v", err)
	}
	if string(page.Data) != "oops" {
		t.Fatalf("stderr page = %q", page.Data)
	}
}

func TestTransportFailureIsAmbiguousNotAbsent(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response is not hijackable")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	_, err := client.InspectContainer(context.Background(), "c")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("inspect transport error = %v; want ErrAmbiguous", err)
	}
	if errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("transport error collapsed into ErrNotFound: %v", err)
	}
}

func TestTimeoutIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.InspectContainer(ctx, "c")
	once.Do(func() { close(release) })
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("timeout error = %v; want ErrAmbiguous", err)
	}
}

func TestCancellationIsAmbiguous(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled request reached the server")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.InspectContainer(ctx, "c"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("pre-cancel error = %v; want ErrAmbiguous", err)
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	socket := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	client, err := NewLibpod(LibpodConfig{SocketPath: socket, MaxResponseBytes: 32})
	if err != nil {
		t.Fatalf("NewLibpod: %v", err)
	}
	if _, err := client.InspectContainer(context.Background(), "c"); err == nil {
		t.Fatal("oversized inspect response was accepted")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestRemoveVolumeAndInspectVolume(t *testing.T) {
	status := http.StatusOK
	var removeQuery url.Values
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, status, map[string]any{
				"Name":   "kodema-workspace-1",
				"Labels": map[string]string{"kodema.dev/workspace": "workspace-1"},
			})
			return
		}
		if r.Method == http.MethodDelete {
			removeQuery = r.URL.Query()
		}
		w.WriteHeader(status)
	}))
	volume, err := client.InspectVolume(context.Background(), "kodema-workspace-1")
	if err != nil {
		t.Fatalf("InspectVolume: %v", err)
	}
	if volume.Name != "kodema-workspace-1" || volume.Labels["kodema.dev/workspace"] != "workspace-1" {
		t.Fatalf("volume = %+v", volume)
	}
	if err := client.RemoveVolume(context.Background(), "kodema-workspace-1"); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	// Volume removal must not force: force would delete every container still
	// using the volume. Podman must refuse an in-use volume instead.
	if removeQuery == nil {
		t.Fatal("volume DELETE was not observed")
	}
	if removeQuery.Has("force") {
		t.Fatalf("volume DELETE query = %q; want no force parameter", removeQuery.Encode())
	}
	status = http.StatusNotFound
	if _, err := client.InspectVolume(context.Background(), "missing"); !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("inspect missing volume error = %v", err)
	}
	if err := client.RemoveVolume(context.Background(), "missing"); !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("remove missing volume error = %v", err)
	}
}

// TestRemoveVolumeInUseIsUncertain proves a Podman 409 conflict (volume still
// mounted) is neither a fabricated absence nor an already-exists sentinel: the
// adapter treats it as an uncertain release and keeps the workspace.
func TestRemoveVolumeInUseIsUncertain(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.WriteHeader(http.StatusConflict)
	}))
	err := client.RemoveVolume(context.Background(), "kodema-workspace-1")
	if err == nil {
		t.Fatal("in-use volume removal reported success")
	}
	if errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("in-use volume error = %v; must not be ErrNotFound", err)
	}
	if errors.Is(err, runtimepodman.ErrAlreadyExists) {
		t.Fatalf("in-use volume error = %v; must not be ErrAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("in-use volume error = %v; want an in-use conflict", err)
	}
}

func TestCreateBodyRejectsMalformedEnvironment(t *testing.T) {
	_, err := createBody(runtimepodman.ContainerSpec{
		Name: "c", Image: "i", Env: []string{"NO_EQUALS"},
	})
	if err == nil {
		t.Fatal("malformed environment entry was accepted")
	}
}

func TestCreateContainerMapsCustomResourceLimits(t *testing.T) {
	var got createSpec
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "container-res"})
	}))
	_, err := client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{
		Name:          "kodema-workload-res",
		Image:         "registry.example.com/tools/build@sha256:" + strings.Repeat("b", 64),
		RestartPolicy: "no",
		Resources: runtimepodman.ContainerResources{
			MemoryBytes: 4 << 30,
			CPUQuota:    350000,
			CPUPeriod:   100000,
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got.ResourceLimits == nil || got.ResourceLimits.Memory == nil || got.ResourceLimits.CPU == nil {
		t.Fatalf("resource limits missing: %+v", got.ResourceLimits)
	}
	if got.ResourceLimits.Memory.Limit != 4<<30 {
		t.Fatalf("memory limit = %d; want %d", got.ResourceLimits.Memory.Limit, int64(4<<30))
	}
	if got.ResourceLimits.CPU.Quota != 350000 || got.ResourceLimits.CPU.Period != 100000 {
		t.Fatalf("cpu limit = %+v; want quota=350000 period=100000", got.ResourceLimits.CPU)
	}
	if _, err := createBody(runtimepodman.ContainerSpec{
		Name:      "c",
		Image:     "i",
		Resources: runtimepodman.ContainerResources{MemoryBytes: -1},
	}); err == nil {
		t.Fatal("negative memory limit was accepted")
	}
}

func TestInspectContainerMapsOOMKilledAndMemoryLimit(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Id":"oom-1",
			"Name":"kodema-workload-oom",
			"ImageName":"img@sha256:x",
			"State":{"Status":"exited","Running":false,"OOMKilled":true,"Pid":0,"ExitCode":137,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-01T00:01:00Z"},
			"HostConfig":{"Memory":2147483648},
			"Config":{"Labels":{"kodema.dev/workload":"wl-oom"}}
		}`))
	}))
	state, err := client.InspectContainer(context.Background(), "kodema-workload-oom")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if !state.OOMKilled || state.ExitCode != 137 || !state.WholeStopped {
		t.Fatalf("state = %+v; want OOMKilled=true ExitCode=137 WholeStopped=true", state)
	}
	if state.MemoryLimitBytes != 2<<30 {
		t.Fatalf("MemoryLimitBytes = %d; want %d", state.MemoryLimitBytes, int64(2<<30))
	}
}

func TestInspectImageMapsPresentNotFoundAndErrors(t *testing.T) {
	const pinned = "registry.example.com/tools/build@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var (
		status    = http.StatusNoContent
		seenPaths []string
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		seenPaths = append(seenPaths, r.URL.EscapedPath())
		w.WriteHeader(status)
	}))

	if err := client.InspectImage(context.Background(), pinned); err != nil {
		t.Fatalf("InspectImage(204): %v", err)
	}
	wantEscaped := "/v5.0.0/libpod/images/" + url.PathEscape(pinned) + "/exists"
	if len(seenPaths) != 1 || seenPaths[0] != wantEscaped {
		t.Fatalf("InspectImage request paths = %v; want [%s]", seenPaths, wantEscaped)
	}

	status = http.StatusOK
	if err := client.InspectImage(context.Background(), pinned); err != nil {
		t.Fatalf("InspectImage(200): %v", err)
	}

	status = http.StatusNotFound
	if err := client.InspectImage(context.Background(), pinned); !errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("InspectImage(404) = %v; want ErrNotFound", err)
	}

	status = http.StatusInternalServerError
	err := client.InspectImage(context.Background(), pinned)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("InspectImage(500) = %v; want dependency_unavailable", err)
	}

	before := len(seenPaths)
	err = client.InspectImage(context.Background(), "   ")
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("InspectImage(empty) = %v; want invalid_input", err)
	}
	if len(seenPaths) != before {
		t.Fatal("empty image reference sent an HTTP request")
	}
}

func TestNewLibpodRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := NewLibpod(LibpodConfig{}); err == nil {
		t.Fatal("empty socket path was accepted")
	}
	if _, err := NewLibpod(LibpodConfig{SocketPath: "relative.sock"}); err == nil {
		t.Fatal("relative socket path was accepted")
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
