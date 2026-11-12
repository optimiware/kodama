// Behavioural specification for the driver-owned workspace claim provisioning
// primitive.
//
// The runtimekube.Cluster seam deliberately has no claim-create method:
// the adapter only observes, retains, updates and deletes a claim that must
// already exist. A composed server therefore cannot provision the workspace PVC
// through the seam. The driver exposes one concrete, seam-external method on
// *Cluster:
//
//	type ClaimSpec struct {
//	    Namespace    string
//	    TenantID     id.TenantID
//	    WorkspaceID  id.WorkspaceID
//	    StorageClass string
//	    Size         string
//	    AccessMode   string
//	}
//
//	func (c *Cluster) CreateClaim(ctx context.Context, spec ClaimSpec) (runtimekube.Claim, error)
//
// The driver derives the object name with runtimekube.WorkspaceClaimName, so a
// retry after a lost response addresses the identical PVC, and stamps exactly
// the two ownership labels the adapter's unownedClaimReason validates
// (kodama.dev/tenant, kodama.dev/workspace). No retention annotation is sent:
// the adapter's Retain owns that marker. AccessMode must be one of the four
// Kubernetes access modes (ReadWriteOnce, ReadOnlyMany, ReadWriteMany,
// ReadWriteOncePod); any other value is refused before sending.
//
// This file pins the request body, the idempotent 409-adopt path, the refusal
// of a foreign or terminating existing claim, the uncertain outcomes that must
// stay ErrAmbiguous, and the pre-send validation. A 2xx response whose identity
// or ownership labels do not describe the requested claim is an untrustworthy
// success and stays ErrAmbiguous, like any other malformed 2xx. It drives only
// an in-process TLS httptest API server; no real cluster is contacted.
package kube_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

const (
	testStorageClass = "standard"
	testClaimSize    = "64Mi"
	testAccessMode   = "ReadWriteOnce"
)

// testClaimSpec builds the storage specification the caller supplies for one
// retained workspace.
func testClaimSpec(workspaceID id.WorkspaceID) driverkube.ClaimSpec {
	return driverkube.ClaimSpec{
		Namespace:    testNamespace,
		TenantID:     id.TenantID(testNamespace),
		WorkspaceID:  workspaceID,
		StorageClass: testStorageClass,
		Size:         testClaimSize,
		AccessMode:   testAccessMode,
	}
}

// claimNameFor returns the deterministic claim name the seam derives.
func claimNameFor(t *testing.T, workspaceID id.WorkspaceID) string {
	t.Helper()
	name, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}
	return name
}

// ownedClaimWire is a server response for the claim this workspace owns.
func ownedClaimWire(name string, workspaceID id.WorkspaceID) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       testNamespace,
			"resourceVersion": "7",
			"labels": map[string]string{
				runtimekube.LabelTenant:    testNamespace,
				runtimekube.LabelWorkspace: string(workspaceID),
			},
			"finalizers": []string{"kubernetes.io/pvc-protection"},
		},
	}
}

// assertAmbiguous proves an uncertain outcome is reported as ErrAmbiguous and
// never as a fabricated absence or an already-exists success.
func assertAmbiguous(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; an uncertain create must never be reported as success")
	}
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error %v does not wrap ErrAmbiguous", err)
	}
	for _, sentinel := range []error{runtimekube.ErrNotFound, runtimekube.ErrAlreadyExists, runtimekube.ErrConflict} {
		if errors.Is(err, sentinel) {
			t.Fatalf("error %v wraps %v; an uncertain create must not borrow another classification", err, sentinel)
		}
	}
}

// TestCreateClaimSendsExactPVCBody proves the driver POSTs exactly the requested
// PVC: the deterministic name, the two ownership labels, no retention
// annotation, and the requested storage spec. It also proves a 2xx response is
// validated and server-added metadata (uid, finalizers, annotations) survives.
func TestCreateClaimSendsExactPVCBody(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	claimName := claimNameFor(t, workspaceID)
	wantBody := fmt.Sprintf(`{
	  "apiVersion": "v1",
	  "kind": "PersistentVolumeClaim",
	  "metadata": {
	    "name": %q,
	    "namespace": %q,
	    "labels": {
	      "kodama.dev/tenant": %q,
	      "kodama.dev/workspace": %q
	    }
	  },
	  "spec": {
	    "accessModes": ["ReadWriteOnce"],
	    "resources": {"requests": {"storage": "64Mi"}},
	    "storageClassName": "standard",
	    "volumeMode": "Filesystem"
	  }
	}`, claimName, testNamespace, testNamespace, string(workspaceID))

	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != claimsPath() {
			t.Errorf("unexpected request %s %s; want POST %s", r.Method, r.URL.Path, claimsPath())
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", contentType)
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":            claimName,
				"namespace":       testNamespace,
				"uid":             "uid-1",
				"resourceVersion": "7",
				"labels": map[string]string{
					runtimekube.LabelTenant:    testNamespace,
					runtimekube.LabelWorkspace: string(workspaceID),
					"server.example/added":     "yes",
				},
				"annotations": map[string]string{
					"volume.kubernetes.io/storage-provisioner": "rancher.io/local-path",
				},
				"finalizers": []string{"kubernetes.io/pvc-protection"},
			},
		})
	})
	cluster := newClusterFor(t, api)

	claim, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if claim.Name != claimName || claim.Namespace != testNamespace {
		t.Fatalf("claim identity = %s/%s; want %s/%s", claim.Namespace, claim.Name, testNamespace, claimName)
	}
	if claim.Labels[runtimekube.LabelTenant] != testNamespace ||
		claim.Labels[runtimekube.LabelWorkspace] != string(workspaceID) {
		t.Fatalf("claim labels = %v; want the two ownership labels", claim.Labels)
	}
	if claim.ResourceVersion != "7" {
		t.Fatalf("claim resourceVersion = %q; want the server value", claim.ResourceVersion)
	}
	if len(claim.Finalizers) != 1 || claim.Finalizers[0] != "kubernetes.io/pvc-protection" {
		t.Fatalf("claim finalizers = %v; want the server-added finalizer", claim.Finalizers)
	}
	if claim.Annotations["volume.kubernetes.io/storage-provisioner"] != "rancher.io/local-path" {
		t.Fatalf("claim annotations = %v; want the server-added annotation", claim.Annotations)
	}

	call := api.lastCall(t)
	assertJSONEqual(t, call.Body, wantBody)
	// The retention marker belongs to the adapter's Retain; a create must never
	// pre-set it. The exact-body comparison above already forbids an annotations
	// object, and this guards the specific marker if the shape changes.
	body := decodeJSON(t, call.Body).(map[string]any)
	metadata := body["metadata"].(map[string]any)
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		if _, present := annotations[runtimekube.AnnotationRetained]; present {
			t.Fatalf("create body carries the retention annotation: %v", annotations)
		}
	}
}

// TestCreateClaimAdoptsOwnedClaimAfterLostResponse proves a create whose
// response was lost is ErrAmbiguous, and that the retry is idempotent: the
// server's 409 is reconciled with a GET and the same-owner claim is adopted.
func TestCreateClaimAdoptsOwnedClaimAfterLostResponse(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	claimName := claimNameFor(t, workspaceID)
	existing := ownedClaimWire(claimName, workspaceID)
	metadata := existing["metadata"].(map[string]any)
	// An existing claim may carry extra labels and a retention annotation set by
	// the adapter; adoption keys only on the two ownership labels.
	metadata["labels"].(map[string]string)["server.example/added"] = "yes"
	metadata["annotations"] = map[string]string{runtimekube.AnnotationRetained: "true"}

	var posts atomic.Int32
	var mu sync.Mutex
	var methods []string
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && posts.Add(1) == 1:
			// The request reached the API server, but the response is lost: the
			// effect may or may not have happened, so it must stay ambiguous.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("fake API server cannot lose a response")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		case r.Method == http.MethodPost:
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
		case r.Method == http.MethodGet && r.URL.Path == claimPath(claimName):
			writeJSON(w, http.StatusOK, existing)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	_, lostErr := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
	assertAmbiguous(t, lostErr)

	claim, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
	if err != nil {
		t.Fatalf("idempotent retry after a lost response: %v", err)
	}
	if claim.Name != claimName || claim.Namespace != testNamespace {
		t.Fatalf("adopted claim = %s/%s; want %s/%s", claim.Namespace, claim.Name, testNamespace, claimName)
	}
	if claim.Labels[runtimekube.LabelTenant] != testNamespace ||
		claim.Labels[runtimekube.LabelWorkspace] != string(workspaceID) {
		t.Fatalf("adopted claim labels = %v; want the two ownership labels", claim.Labels)
	}
	if claim.Annotations[runtimekube.AnnotationRetained] != "true" {
		t.Fatalf("adopted claim annotations = %v; adoption must preserve the retention marker", claim.Annotations)
	}

	mu.Lock()
	got := append([]string(nil), methods...)
	mu.Unlock()
	want := []string{
		"POST " + claimsPath(),
		"POST " + claimsPath(),
		"GET " + claimPath(claimName),
	}
	if len(got) != len(want) {
		t.Fatalf("request sequence = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("request sequence = %v; want %v", got, want)
		}
	}
}

// TestCreateClaimRefusesForeignExistingClaim proves a create conflict is
// adopted only for a claim carrying both ownership labels. A foreign,
// differently-owned or unlabeled claim is Forbidden and is never overwritten.
func TestCreateClaimRefusesForeignExistingClaim(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{
			name: "different tenant",
			labels: map[string]string{
				runtimekube.LabelTenant:    "tenant-other",
				runtimekube.LabelWorkspace: "workspace-1",
			},
		},
		{
			name: "different workspace",
			labels: map[string]string{
				runtimekube.LabelTenant:    testNamespace,
				runtimekube.LabelWorkspace: "workspace-2",
			},
		},
		{
			name:   "unlabeled",
			labels: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspaceID := id.WorkspaceID("workspace-1")
			claimName := claimNameFor(t, workspaceID)
			api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
				case http.MethodGet:
					writeJSON(w, http.StatusOK, map[string]any{
						"metadata": map[string]any{
							"name":            claimName,
							"namespace":       testNamespace,
							"resourceVersion": "3",
							"labels":          tc.labels,
						},
					})
				default:
					t.Errorf("unexpected request %s %s; a foreign claim must never be overwritten", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
				}
			})
			cluster := newClusterFor(t, api)

			_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
			if err == nil {
				t.Fatal("CreateClaim adopted a foreign claim")
			}
			assertFaultClass(t, err, fault.Forbidden)
			if errors.Is(err, runtimekube.ErrAlreadyExists) {
				t.Fatalf("error %v leaked ErrAlreadyExists as the outcome", err)
			}
			calls := api.snapshot()
			if len(calls) != 2 || calls[0].Method != http.MethodPost || calls[1].Method != http.MethodGet {
				t.Fatalf("calls = %s; want exactly POST then GET", formatCalls(calls))
			}
		})
	}
}

// TestCreateClaimRefusesTerminatingExistingClaim proves a same-owner claim that
// is already terminating is never adopted as a usable workspace: it is a
// conflict, not success.
func TestCreateClaimRefusesTerminatingExistingClaim(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	claimName := claimNameFor(t, workspaceID)
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":              claimName,
					"namespace":         testNamespace,
					"resourceVersion":   "9",
					"deletionTimestamp": "2026-11-06T10:01:00Z",
					"labels": map[string]string{
						runtimekube.LabelTenant:    testNamespace,
						runtimekube.LabelWorkspace: string(workspaceID),
					},
					"finalizers": []string{"kubernetes.io/pvc-protection"},
				},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	claim, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
	if err == nil {
		t.Fatalf("CreateClaim reported success for a terminating claim: %+v", claim)
	}
	assertFaultClass(t, err, fault.VersionConflict)
	if !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("error %v does not wrap ErrConflict", err)
	}
}

// TestCreateClaimRefusesInvalidSpecBeforeSending proves every incomplete or
// malformed specification is refused before the API server sees a request.
func TestCreateClaimRefusesInvalidSpecBeforeSending(t *testing.T) {
	base := testClaimSpec(id.WorkspaceID("workspace-1"))
	cases := []struct {
		name   string
		mutate func(*driverkube.ClaimSpec)
	}{
		{name: "empty namespace", mutate: func(spec *driverkube.ClaimSpec) { spec.Namespace = "" }},
		{name: "empty tenant", mutate: func(spec *driverkube.ClaimSpec) { spec.TenantID = "" }},
		{name: "invalid tenant", mutate: func(spec *driverkube.ClaimSpec) { spec.TenantID = "tenant/acme" }},
		{name: "empty workspace", mutate: func(spec *driverkube.ClaimSpec) { spec.WorkspaceID = "" }},
		{name: "invalid workspace", mutate: func(spec *driverkube.ClaimSpec) { spec.WorkspaceID = "workspace 1" }},
		{name: "empty storage class", mutate: func(spec *driverkube.ClaimSpec) { spec.StorageClass = "" }},
		{name: "empty size", mutate: func(spec *driverkube.ClaimSpec) { spec.Size = "" }},
		{name: "empty access mode", mutate: func(spec *driverkube.ClaimSpec) { spec.AccessMode = "" }},
		{name: "unknown access mode", mutate: func(spec *driverkube.ClaimSpec) { spec.AccessMode = "ReadWriteTwice" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("invalid spec reached the API server: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})
			cluster := newClusterFor(t, api)
			spec := base
			tc.mutate(&spec)
			_, err := cluster.CreateClaim(context.Background(), spec)
			assertFaultClass(t, err, fault.InvalidInput)
			if calls := api.callCount(); calls != 0 {
				t.Fatalf("%d request(s) sent for an invalid spec; want none", calls)
			}
		})
	}
}

// TestCreateClaimConflictWithUnreadableClaimIsAmbiguous proves a create
// conflict whose existing claim cannot be read is an uncertain outcome. The 409
// already proves the object existed, so a failed reconciliation must never
// become a fabricated absence, success or already-exists outcome.
func TestCreateClaimConflictWithUnreadableClaimIsAmbiguous(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	cases := []struct {
		name   string
		status int
	}{
		{name: "claim disappeared", status: http.StatusNotFound},
		{name: "claim read failed", status: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
				case http.MethodGet:
					writeJSON(w, tc.status, map[string]any{"kind": "Status"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
				}
			})
			cluster := newClusterFor(t, api)

			_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
			assertAmbiguous(t, err)
			calls := api.snapshot()
			if len(calls) != 2 || calls[0].Method != http.MethodPost || calls[1].Method != http.MethodGet {
				t.Fatalf("calls = %s; want exactly POST then GET", formatCalls(calls))
			}
		})
	}
}

// TestCreateClaimUncertainOutcomes proves a cancellation, server failure,
// redirect, timeout or malformed/mismatched 2xx is ErrAmbiguous and never a
// fabricated success, absence or already-exists outcome. Error text never
// renders the bearer token even when the response body contains it.
func TestCreateClaimUncertainOutcomes(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	claimName := claimNameFor(t, workspaceID)

	t.Run("cancelled before send", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("cancelled call reached the API server: %s %s", r.Method, r.URL.Path)
		})
		cluster := newClusterFor(t, api)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := cluster.CreateClaim(ctx, testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v does not carry the cancellation", err)
		}
		if calls := api.callCount(); calls != 0 {
			t.Fatalf("%d request(s) sent after cancellation; want none", calls)
		}
	})

	t.Run("server failure with token in body", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"kind":    "Status",
				"message": "internal error for " + testToken,
			})
		})
		cluster := newClusterFor(t, api)
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
		assertNoCredential(t, err, testToken)
	})

	t.Run("redirect refused", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://example.invalid/redirected")
			w.WriteHeader(http.StatusFound)
		})
		cluster := newClusterFor(t, api)
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
		if calls := api.callCount(); calls != 1 {
			t.Fatalf("%d request(s); a redirect must be refused, not followed", calls)
		}
	})

	t.Run("malformed success", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":`))
		})
		cluster := newClusterFor(t, api)
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
	})

	t.Run("mismatched returned name", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			response := ownedClaimWire("kodama-workspace-someone-else", workspaceID)
			writeJSON(w, http.StatusCreated, response)
		})
		cluster := newClusterFor(t, api)
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
	})

	t.Run("mismatched returned ownership label", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			response := ownedClaimWire(claimName, workspaceID)
			response["metadata"].(map[string]any)["labels"].(map[string]string)[runtimekube.LabelWorkspace] = "workspace-2"
			writeJSON(w, http.StatusCreated, response)
		})
		cluster := newClusterFor(t, api)
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
	})

	t.Run("timeout", func(t *testing.T) {
		api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		cluster := newClusterWith(t, api.kubeconfig(t, nil, nil), func(config *driverkube.ClusterConfig) {
			config.Timeout = 150 * time.Millisecond
		})
		_, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
		assertAmbiguous(t, err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error %v does not carry the deadline", err)
		}
	})
}

// TestCreateClaimConcurrentCreateConvergesOnOneClaim proves two concurrent
// creates of the same workspace converge on the one deterministic claim: one
// 201 and one 409-then-GET adoption both succeed with the same identity.
func TestCreateClaimConcurrentCreateConvergesOnOneClaim(t *testing.T) {
	workspaceID := id.WorkspaceID("workspace-1")
	claimName := claimNameFor(t, workspaceID)
	existing := ownedClaimWire(claimName, workspaceID)
	var posts atomic.Int32
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if posts.Add(1) == 1 {
				writeJSON(w, http.StatusCreated, existing)
				return
			}
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
		case http.MethodGet:
			writeJSON(w, http.StatusOK, existing)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	type outcome struct {
		claim runtimekube.Claim
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			claim, err := cluster.CreateClaim(context.Background(), testClaimSpec(workspaceID))
			results <- outcome{claim: claim, err: err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent CreateClaim: %v", result.err)
		}
		if result.claim.Name != claimName || result.claim.Namespace != testNamespace {
			t.Fatalf("concurrent CreateClaim returned %s/%s; want %s/%s",
				result.claim.Namespace, result.claim.Name, testNamespace, claimName)
		}
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("POST count = %d; want 2 (one create and one retry)", got)
	}
}
