package podman

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// workspaceVolume returns the exact volume the Shell must provision before the
// Kernel retains a workspace: the deterministic name plus the ownership label
// the adapter's ownsWorkspaceVolume validates.
func workspaceVolume(name, workspaceID string) runtimepodman.Volume {
	return runtimepodman.Volume{
		Name:   name,
		Labels: map[string]string{runtimepodman.LabelWorkspace: workspaceID},
	}
}

// TestCreateVolumeSendsExactNameAndLabels pins the libpod volume-create
// request: exactly the volume name and the ownership labels, no extra driver or
// option fields that could change where the workspace data lives.
func TestCreateVolumeSendsExactNameAndLabels(t *testing.T) {
	requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
	var got map[string]json.RawMessage
	var method, path string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode volume create body: %v", err)
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"Name":   requested.Name,
			"Labels": requested.Labels,
			"Driver": "local",
		})
	}))
	created, err := client.CreateVolume(context.Background(), requested)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if method != http.MethodPost || path != "/v5.0.0/libpod/volumes/create" {
		t.Fatalf("volume create request = %s %s", method, path)
	}
	if len(got) != 2 {
		t.Fatalf("volume create body keys = %v; want exactly Name and Labels", keys(got))
	}
	var name string
	if err := json.Unmarshal(got["Name"], &name); err != nil {
		t.Fatalf("decode body Name: %v", err)
	}
	if name != requested.Name {
		t.Fatalf("body Name = %q; want %q", name, requested.Name)
	}
	var labels map[string]string
	if err := json.Unmarshal(got["Labels"], &labels); err != nil {
		t.Fatalf("decode body Labels: %v", err)
	}
	if len(labels) != 1 || labels[runtimepodman.LabelWorkspace] != "workspace-abc" {
		t.Fatalf("body Labels = %v; want the workspace ownership label", labels)
	}
	if created.Name != requested.Name {
		t.Fatalf("created volume name = %q; want %q", created.Name, requested.Name)
	}
	if created.Labels[runtimepodman.LabelWorkspace] != "workspace-abc" {
		t.Fatalf("created volume labels = %v; want the ownership label", created.Labels)
	}
}

// TestCreateVolumeIdempotentSameOwner proves a 409/exists response is
// reconciled by inspecting the existing volume: when it already carries the
// requested ownership labels (extra run labels are harmless) the create is
// idempotent success, never a second volume or an error.
func TestCreateVolumeIdempotentSameOwner(t *testing.T) {
	requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
	inspections := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusConflict)
		case http.MethodGet:
			inspections++
			if r.URL.Path != "/v5.0.0/libpod/volumes/kodama-workspace-abc/json" {
				t.Errorf("inspect path = %s", r.URL.Path)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"Name": requested.Name,
				"Labels": map[string]string{
					runtimepodman.LabelWorkspace: "workspace-abc",
					"kodama.test":                "1",
				},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	created, err := client.CreateVolume(context.Background(), requested)
	if err != nil {
		t.Fatalf("idempotent CreateVolume: %v", err)
	}
	if inspections != 1 {
		t.Fatalf("existing volume inspections = %d; want exactly 1", inspections)
	}
	if created.Name != requested.Name {
		t.Fatalf("adopted volume name = %q; want %q", created.Name, requested.Name)
	}
	if created.Labels[runtimepodman.LabelWorkspace] != "workspace-abc" {
		t.Fatalf("adopted volume labels = %v; want the ownership label", created.Labels)
	}
}

// TestCreateVolumeIdempotentDuplicateServerError pins the real libpod duplicate
// conflict: Podman 5.x reports an existing volume as a 500 whose error cause is
// "volume already exists", not 409. Exactly that documented cause is reconciled
// by inspecting the volume, so a retry after a lost create response is
// idempotent against the real daemon too.
func TestCreateVolumeIdempotentDuplicateServerError(t *testing.T) {
	requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
	inspections := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"cause":    "volume already exists",
				"message":  "volume with name " + requested.Name + " already exists: volume already exists",
				"response": http.StatusInternalServerError,
			})
		case http.MethodGet:
			inspections++
			writeJSON(w, http.StatusOK, map[string]any{
				"Name":   requested.Name,
				"Labels": map[string]string{runtimepodman.LabelWorkspace: "workspace-abc"},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	created, err := client.CreateVolume(context.Background(), requested)
	if err != nil {
		t.Fatalf("duplicate create conflict: %v", err)
	}
	if inspections != 1 {
		t.Fatalf("existing volume inspections = %d; want exactly 1", inspections)
	}
	if created.Name != requested.Name {
		t.Fatalf("adopted volume name = %q; want %q", created.Name, requested.Name)
	}
	if created.Labels[runtimepodman.LabelWorkspace] != "workspace-abc" {
		t.Fatalf("adopted volume labels = %v; want the ownership label", created.Labels)
	}
}

// TestCreateVolumeDuplicateServerErrorRefusesForeignOwner proves the 500
// duplicate conflict applies the same ownership check as 409: a same-named
// volume with different labels is Forbidden, never adopted.
func TestCreateVolumeDuplicateServerErrorRefusesForeignOwner(t *testing.T) {
	requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"cause":    "volume already exists",
				"message":  "volume with name " + requested.Name + " already exists: volume already exists",
				"response": http.StatusInternalServerError,
			})
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"Name":   requested.Name,
				"Labels": map[string]string{runtimepodman.LabelWorkspace: "workspace-other"},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	created, err := client.CreateVolume(context.Background(), requested)
	if err == nil {
		t.Fatalf("foreign volume was adopted: %+v", created)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("foreign duplicate error class = %s/%v; want forbidden (error %v)", class, ok, err)
	}
}

// TestCreateVolumeUnrelatedServerErrorIsDependencyFailure proves only the
// documented duplicate conflict is reconciled: any other 5xx is a dependency
// failure, and a same-named volume is not adopted from it.
func TestCreateVolumeUnrelatedServerErrorIsDependencyFailure(t *testing.T) {
	requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
	inspections := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"cause":    "storage is not configured",
				"message":  "volume driver failed",
				"response": http.StatusInternalServerError,
			})
		case http.MethodGet:
			inspections++
			writeJSON(w, http.StatusOK, map[string]any{
				"Name":   requested.Name,
				"Labels": map[string]string{runtimepodman.LabelWorkspace: "workspace-abc"},
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	created, err := client.CreateVolume(context.Background(), requested)
	if err == nil {
		t.Fatalf("unrelated server error adopted a volume: %+v", created)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("unrelated server error class = %s/%v; want dependency_unavailable (error %v)", class, ok, err)
	}
	if inspections != 0 {
		t.Fatalf("existing volume inspections = %d; want 0 for an unrelated server failure", inspections)
	}
}

// TestCreateVolumeRefusesForeignOwner proves a same-named volume with a
// different or missing ownership label is never silently adopted. The create
// must fail as a Forbidden fault, not report success and not pretend the
// foreign volume is the requested one.
func TestCreateVolumeRefusesForeignOwner(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{name: "different workspace", labels: map[string]string{runtimepodman.LabelWorkspace: "workspace-other"}},
		{name: "missing ownership label", labels: map[string]string{"kodama.test": "1"}},
		{name: "no labels", labels: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requested := workspaceVolume("kodama-workspace-abc", "workspace-abc")
			inspections := 0
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					w.WriteHeader(http.StatusConflict)
				case http.MethodGet:
					inspections++
					writeJSON(w, http.StatusOK, map[string]any{
						"Name":   requested.Name,
						"Labels": tc.labels,
					})
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			created, err := client.CreateVolume(context.Background(), requested)
			if err == nil {
				t.Fatalf("foreign volume was adopted: %+v", created)
			}
			if inspections != 1 {
				t.Fatalf("existing volume inspections = %d; want exactly 1", inspections)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.Forbidden {
				t.Fatalf("foreign volume error class = %s/%v; want forbidden (error %v)", class, ok, err)
			}
			if errors.Is(err, runtimepodman.ErrAlreadyExists) {
				t.Fatalf("foreign volume error = %v; must not claim the caller's volume already exists", err)
			}
		})
	}
}

// TestCreateVolumeCancellationBeforeSend proves a cancelled context performs no
// request and surfaces the seam's ambiguous classification instead of a
// fabricated create.
func TestCreateVolumeCancellationBeforeSend(t *testing.T) {
	reached := false
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.CreateVolume(ctx, workspaceVolume("kodama-workspace-abc", "workspace-abc")); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("pre-cancel error = %v; want ErrAmbiguous", err)
	}
	if reached {
		t.Fatal("cancelled volume create reached the server")
	}
}

// TestCreateVolumeTransportFailureIsAmbiguous proves a failure after the
// request was sent is uncertain: it is ErrAmbiguous, never success and never
// the already-exists sentinel that would let the caller assume the volume is
// its own.
func TestCreateVolumeTransportFailureIsAmbiguous(t *testing.T) {
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
	_, err := client.CreateVolume(context.Background(), workspaceVolume("kodama-workspace-abc", "workspace-abc"))
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("transport error = %v; want ErrAmbiguous", err)
	}
	if errors.Is(err, runtimepodman.ErrAlreadyExists) || errors.Is(err, runtimepodman.ErrNotFound) {
		t.Fatalf("transport error collapsed into a fabricated object state: %v", err)
	}
}

// TestCreateVolumeServerErrorIsDependencyFailure proves a 5xx is a dependency
// failure, not success and not a claim about the volume's existence.
func TestCreateVolumeServerErrorIsDependencyFailure(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_, err := client.CreateVolume(context.Background(), workspaceVolume("kodama-workspace-abc", "workspace-abc"))
	if err == nil {
		t.Fatal("volume create 500 reported success")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("volume create 500 error class = %s/%v; want dependency_unavailable (error %v)", class, ok, err)
	}
	if errors.Is(err, runtimepodman.ErrAlreadyExists) {
		t.Fatalf("volume create 500 error = %v; must not claim already exists", err)
	}
}

// TestCreateVolumeMalformedResponseIsRefused proves a success status with an
// undecodable body is a dependency failure, not an adopted empty volume.
func TestCreateVolumeMalformedResponseIsRefused(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Name":`))
	}))
	created, err := client.CreateVolume(context.Background(), workspaceVolume("kodama-workspace-abc", "workspace-abc"))
	if err == nil {
		t.Fatalf("malformed create response was accepted: %+v", created)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("malformed response error class = %s/%v; want dependency_unavailable (error %v)", class, ok, err)
	}
}

// TestCreateVolumeRejectsEmptyName proves the driver refuses an unaddressable
// volume before sending anything.
func TestCreateVolumeRejectsEmptyName(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("empty volume name reached the server")
	}))
	if _, err := client.CreateVolume(context.Background(), runtimepodman.Volume{
		Labels: map[string]string{runtimepodman.LabelWorkspace: "workspace-abc"},
	}); err == nil {
		t.Fatal("empty volume name was accepted")
	} else if !strings.Contains(err.Error(), "name") {
		t.Fatalf("empty name error = %v; want a name validation error", err)
	}
}

func keys(body map[string]json.RawMessage) []string {
	out := make([]string, 0, len(body))
	for key := range body {
		out = append(out, key)
	}
	return out
}
