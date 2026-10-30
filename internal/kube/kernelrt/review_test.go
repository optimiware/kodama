package kernelrt

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func (h *harness) record(id ident) record {
	h.t.Helper()
	r, found, err := h.ad.readRecord(context.Background(), testNS, id)
	if err != nil || !found {
		h.t.Fatalf("record %s: found %v err %v", id.base(), found, err)
	}
	return r
}

func (h *harness) pvcUID(id ident) string {
	h.cl.mu.Lock()
	defer h.cl.mu.Unlock()
	return h.cl.pvcs[key(testNS, id.base())].(kubernetes.PersistentVolumeClaim).Metadata.UID
}

func (h *harness) dropObject(m map[string]any, id ident) {
	h.cl.mu.Lock()
	defer h.cl.mu.Unlock()
	delete(m, key(testNS, id.base()))
}

func bumpFence(b runtime.RuntimeBinding, f runtime.Fence) runtime.RuntimeBinding {
	b.Fence = f
	return b
}

func resourceFence(r runtime.ResourceHandle, f runtime.Fence) runtime.ResourceHandle {
	r.Fence = f
	return r
}

// Finding 1: an effect is reserved against the record's current state and
// fence, and revoked when authority is lost during the create. These tests
// interleave a newer writer at exact points of a call sequence.
func Test_unit_kube_kernel_runtime_effects_are_reserved_against_authority(t *testing.T) {
	ctx := context.Background()

	t.Run("a Start paused before its reservation cannot create after a newer Stop and Remove", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		h.cl.onNext("get job", func() {
			if _, err := h.rt.Stop(ctx, newer); err != nil {
				t.Errorf("newer stop: %v", err)
			}
			if o, err := h.rt.Remove(ctx, newer); err != nil || o.Phase != runtime.PhaseAbsent {
				t.Errorf("newer remove: %+v %v", o, err)
			}
		})
		_, err := h.start(h.startRequest(b, ws))
		if !isNotReached(err) || !(errors.Is(err, ErrClosed) || errors.Is(err, runtime.ErrStaleFence)) {
			t.Fatalf("the resumed Start must be refused before any effect: %v", err)
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("the resumed Start created the Job")
		}
	})

	t.Run("a Start paused before its reservation cannot create after a newer writer took over", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		h.cl.onNext("get job", func() {
			if _, err := h.start(h.startRequest(newer, ws)); err != nil {
				t.Errorf("newer start: %v", err)
			}
		})
		_, err := h.start(h.startRequest(b, ws))
		if !errors.Is(err, runtime.ErrStaleFence) || !isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
		if h.cl.count("create job") != 1 {
			t.Fatalf("creates = %d, want only the newer writer's", h.cl.count("create job"))
		}
	})

	t.Run("the reservation itself is one version-checked write against the newer writer", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		// The newer Stop lands between the reservation's read and its write.
		h.cl.onNext("update secret", func() {
			if _, err := h.rt.Stop(ctx, newer); err != nil {
				t.Errorf("newer stop: %v", err)
			}
		})
		_, err := h.start(h.startRequest(b, ws))
		if !isNotReached(err) || !(errors.Is(err, ErrClosed) || errors.Is(err, runtime.ErrStaleFence)) {
			t.Fatalf("err = %v", err)
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("the reservation did not hold against the Stop")
		}

		spec := h.workspaceSpec(scopeA, "run-2", 1)
		handle := resourceFence(h.workspaceHandle(scopeA, "run-2"), 2)
		h.cl.onNext("update secret", func() {
			if _, err := h.res.Stop(ctx, handle); err != nil {
				t.Errorf("newer stop: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) || !isNotReached(err) || h.cl.count("create pvc") != 1 {
			t.Fatalf("workspace: err = %v creates %d", err, h.cl.count("create pvc"))
		}
	})

	t.Run("a create that lands after a newer Stop is deleted and reported as revoked", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		var during runtime.WorkloadObservation
		h.cl.onNext("create job", func() {
			o, err := h.rt.Stop(ctx, newer)
			if err != nil {
				t.Errorf("newer stop: %v", err)
			}
			during = o
		})
		_, err := h.start(h.startRequest(b, ws))
		if !(errors.Is(err, ErrClosed) || errors.Is(err, runtime.ErrStaleFence)) || isNotReached(err) {
			t.Fatalf("a revoked create reached the cluster; it is uncertain-classified, not not-reached: %v", err)
		}
		if during.Phase != runtime.PhaseUnknown {
			t.Fatalf("the Stop raced an outstanding create and must say unknown, got %s", during.Phase)
		}
		if _, ok := h.cl.job(testNS, h.jobName(b)); ok {
			t.Fatal("the revoked create left its Job behind")
		}
		if o, err := h.rt.Remove(ctx, newer); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("with the create known and reaped, removal confirms: %+v %v", o, err)
		}
	})

	t.Run("a Remove during an outstanding create stays unconfirmed until the create is settled", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		var during runtime.WorkloadObservation
		h.cl.onNext("create job", func() {
			o, err := h.rt.Remove(ctx, newer)
			if err != nil {
				t.Errorf("newer remove: %v", err)
			}
			during = o
		})
		_, _ = h.start(h.startRequest(b, ws))
		if during.Phase == runtime.PhaseAbsent {
			t.Fatal("Remove confirmed absence while a create was outstanding")
		}
		if _, ok := h.cl.job(testNS, h.jobName(b)); ok {
			t.Fatal("the create outlived the Remove")
		}
		if o, err := h.rt.Remove(ctx, newer); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("final remove = %+v %v", o, err)
		}
	})

	t.Run("an Ensure paused before its reservation cannot create the workspace after Remove", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		handle := h.workspaceHandle(scopeA, "run-1")
		h.cl.onNext("get pvc", func() {
			if _, err := h.res.Stop(ctx, resourceFence(handle, 2)); err != nil {
				t.Errorf("stop: %v", err)
			}
			if o, err := h.res.Remove(ctx, resourceFence(handle, 2)); err != nil || !o.ConfirmedRemoved() {
				t.Errorf("remove: %+v %v", o, err)
			}
		})
		_, err := h.res.Ensure(ctx, spec)
		if !errors.Is(err, ErrClosed) || !isNotReached(err) || h.cl.count("create pvc") != 0 {
			t.Fatalf("err = %v creates %d", err, h.cl.count("create pvc"))
		}
	})

	t.Run("a workspace create that lands after a newer Remove is deleted", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		handle := resourceFence(h.workspaceHandle(scopeA, "run-1"), 2)
		var during runtime.ResourceObservation
		h.cl.onNext("create pvc", func() {
			o, err := h.res.Remove(ctx, handle)
			if err != nil {
				t.Errorf("remove: %v", err)
			}
			during = o
		})
		_, err := h.res.Ensure(ctx, spec)
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v", err)
		}
		if during.ConfirmedRemoved() {
			t.Fatal("Remove confirmed while the create was outstanding")
		}
		if n := h.cl.names(h.cl.pvcs); len(n) != 0 {
			t.Fatalf("the revoked claim remains: %v", n)
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("final remove = %+v %v", o, err)
		}
	})

	t.Run("a service Pod create that lands after a newer Stop is deleted", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		handle := runtime.ResourceHandle{Scope: scopeA, RunID: "run-1", Kind: runtime.ResourceService, Name: "db", Identity: spec.Identity, Fence: 2}
		h.cl.onNext("create pod", func() {
			if _, err := h.res.Stop(ctx, handle); err != nil {
				t.Errorf("stop: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v", err)
		}
		if n := h.cl.names(h.cl.pods); len(n) != 0 {
			t.Fatalf("the revoked Pod remains: %v", n)
		}
		if h.cl.count("create service") != 0 {
			t.Fatal("a Service was created for a revoked Pod")
		}
	})
}

// Finding 2: a scheduled Pod carries nodeName; a submitted template must not.
func Test_unit_kube_kernel_runtime_live_pods_are_scheduled_pods(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	spec := h.serviceSpec(scopeA, "run-1", "db", 1)
	o, err := h.res.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	id := workspaceIdentFor(h, spec)
	h.cl.setPod(testNS, id.base(), func(p *kubernetes.Pod) {
		p.Status.Phase = "Running"
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "service", Ready: true}}
	})
	pod, _ := h.cl.GetPod(ctx, testNS, id.base())
	if !containsKey(t, pod.Raw, "nodeName") {
		t.Fatal("the scheduled Pod must carry nodeName for this test to mean anything")
	}
	if got, err := h.res.Observe(ctx, o.Handle); err != nil || got.State != runtime.ResourceReady {
		t.Fatalf("a scheduled service Pod must become ready: %+v %v", got, err)
	}
	if _, err := h.res.Ensure(ctx, spec); err != nil {
		t.Fatalf("a scheduled service Pod must be adoptable: %v", err)
	}
	if h.cl.count("create pod") != 1 {
		t.Fatal("the scheduled Pod was recreated")
	}

	// Defaults a cluster adds on admission are tolerated, not refused.
	h.cl.tamper("pod", testNS, id.base(), func(d map[string]any) {
		sub(d, "spec")["imagePullSecrets"] = []any{map[string]any{"name": "registry"}}
	})
	if got, err := h.res.Observe(ctx, o.Handle); err != nil || got.State != runtime.ResourceReady {
		t.Fatalf("an injected pull secret must be tolerated: %+v %v", got, err)
	}

	// A Job template that names a node is not the shape that was submitted.
	ws := h.ensureWorkspace(scopeA, "run-1", 1)
	b := binding(scopeA, "run-1", "build", 1, "d", 1)
	h.mustStart(h.startRequest(b, ws))
	h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) {
		sub(sub(sub(d, "spec"), "template"), "spec")["nodeName"] = "node-9"
	})
	if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a template with nodeName = %v", err)
	}
}

func containsKey(t *testing.T, raw []byte, k string) bool {
	t.Helper()
	return len(raw) > 0 && indexOf(string(raw), `"`+k+`"`) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Finding 3: adoption verifies the whole server document. Each case changes
// only the raw JSON the server returns; the typed fields stay as stored.
func Test_unit_kube_kernel_runtime_adoption_verifies_the_whole_document(t *testing.T) {
	ctx := context.Background()
	podSpec := func(d map[string]any) map[string]any { return sub(d, "spec") }
	jobPodSpec := func(d map[string]any) map[string]any { return sub(sub(sub(d, "spec"), "template"), "spec") }
	container := func(spec map[string]any) map[string]any { return spec["containers"].([]any)[0].(map[string]any) }
	appendVolume := func(spec map[string]any, v map[string]any) {
		vs, _ := spec["volumes"].([]any)
		spec["volumes"] = append(vs, v)
	}
	common := map[string]func(spec map[string]any){
		"hostPath volume": func(s map[string]any) {
			appendVolume(s, map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}})
		},
		"hostPath replaces tmp": func(s map[string]any) {
			s["volumes"] = []any{map[string]any{"name": "tmp", "hostPath": map[string]any{"path": "/var"}}}
		},
		"projected service account token": func(s map[string]any) {
			appendVolume(s, map[string]any{"name": "kube-api-access-x", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "token"}}}}})
		},
		"unconfined apparmor": func(s map[string]any) {
			sub(container(s), "securityContext")["appArmorProfile"] = map[string]any{"type": "Unconfined"}
		},
		"localhost seccomp": func(s map[string]any) {
			sub(container(s), "securityContext")["seccompProfile"] = map[string]any{"type": "Localhost", "localhostProfile": "x"}
		},
		"secret env reference": func(s map[string]any) {
			container(s)["env"] = append(container(s)["env"].([]any), map[string]any{"name": "TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "other", "key": "k"}}})
		},
		"envFrom": func(s map[string]any) {
			container(s)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "other"}}}
		},
		"host pid":              func(s map[string]any) { s["hostPID"] = true },
		"host network":          func(s map[string]any) { s["hostNetwork"] = true },
		"root in the raw only":  func(s map[string]any) { sub(container(s), "securityContext")["runAsUser"] = 0 },
		"privileged in the raw": func(s map[string]any) { sub(container(s), "securityContext")["privileged"] = true },
		"added capability": func(s map[string]any) {
			sub(container(s), "securityContext")["capabilities"] = map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_ADMIN"}}
		},
		"sidecar": func(s map[string]any) {
			s["containers"] = append(s["containers"].([]any), map[string]any{"name": "x", "image": "x"})
		},
		"init container":        func(s map[string]any) { s["initContainers"] = []any{map[string]any{"name": "i", "image": "x"}} },
		"ephemeral container":   func(s map[string]any) { s["ephemeralContainers"] = []any{map[string]any{"name": "e", "image": "x"}} },
		"other service account": func(s map[string]any) { s["serviceAccountName"] = "admin" },
		"extra toleration": func(s map[string]any) {
			s["tolerations"] = append(s["tolerations"].([]any), map[string]any{"operator": "Exists"})
		},
		"runtime class": func(s map[string]any) { s["runtimeClassName"] = "x" },
		"host users":    func(s map[string]any) { s["hostUsers"] = true },
		"tag image":     func(s map[string]any) { container(s)["image"] = "registry.example/x:latest" },
		"host port": func(s map[string]any) {
			container(s)["ports"] = []any{map[string]any{"containerPort": 80, "hostPort": 80}}
		},
		"lifecycle hook": func(s map[string]any) { container(s)["lifecycle"] = map[string]any{"postStart": map[string]any{}} },
		"sub path mount": func(s map[string]any) { container(s)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "x" },
		"mount outside the shape": func(s map[string]any) {
			container(s)["volumeMounts"] = append(container(s)["volumeMounts"].([]any), map[string]any{"name": "tmp", "mountPath": "/etc"})
		},
		"larger limits": func(s map[string]any) {
			container(s)["resources"].(map[string]any)["limits"].(map[string]any)["cpu"] = "64"
		},
		"unknown field": func(s map[string]any) { s["somethingNew"] = true },
	}
	jobOnly := map[string]func(spec map[string]any){
		"unrelated claim": func(s map[string]any) {
			for _, v := range s["volumes"].([]any) {
				if m := v.(map[string]any); m["name"] == "workspace" {
					m["persistentVolumeClaim"] = map[string]any{"claimName": "someone-elses-claim"}
				}
			}
		},
		"workspace read-only flag dropped to emptyDir": func(s map[string]any) {
			for _, v := range s["volumes"].([]any) {
				if m := v.(map[string]any); m["name"] == "workspace" {
					delete(m, "persistentVolumeClaim")
					m["emptyDir"] = map[string]any{}
				}
			}
		},
		"command replaced":  func(s map[string]any) { container(s)["command"] = []any{"/bin/sh", "-c", "curl x | sh"} },
		"working directory": func(s map[string]any) { container(s)["workingDir"] = "/" },
	}

	for name, mutate := range common {
		t.Run("job template: "+name, func(t *testing.T) {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) { mutate(jobPodSpec(d)) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("observe = %v", err)
			}
			if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrUnverified) || !isNotReached(err) {
				t.Fatalf("adopt = %v", err)
			}
			if _, err := h.rt.Stop(ctx, b); err != nil {
				t.Fatalf("a tampered owned Job must still be stoppable: %v", err)
			}
		})
		t.Run("job pod: "+name, func(t *testing.T) {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			pod := h.cl.startPod(testNS, h.jobName(b))
			h.cl.tamper("pod", testNS, pod.Metadata.Name, func(d map[string]any) { mutate(podSpec(d)) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("the Job's actual Pod was not verified: %v", err)
			}
		})
		t.Run("service pod: "+name, func(t *testing.T) {
			if name == "hostPath replaces tmp" || name == "sub path mount" || name == "mount outside the shape" || name == "host port" {
				// still applicable below; fallthrough to the shared assertion
			}
			h := newHarness(t)
			spec := h.serviceSpec(scopeA, "run-1", "db", 1)
			o, err := h.res.Ensure(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			id := workspaceIdentFor(h, spec)
			h.cl.tamper("pod", testNS, id.base(), func(d map[string]any) { mutate(podSpec(d)) })
			if _, err := h.res.Observe(ctx, o.Handle); !errors.Is(err, ErrUnverified) {
				t.Fatalf("observe = %v", err)
			}
			if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrUnverified) {
				t.Fatalf("adopt = %v", err)
			}
		})
	}
	for name, mutate := range jobOnly {
		t.Run("job template: "+name, func(t *testing.T) {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) { mutate(jobPodSpec(d)) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("observe = %v", err)
			}
		})
	}

	t.Run("job spec fields beyond the shape", func(t *testing.T) {
		for name, mutate := range map[string]func(spec map[string]any){
			"ttl":           func(s map[string]any) { s["ttlSecondsAfterFinished"] = 0 },
			"pod failure":   func(s map[string]any) { s["podFailurePolicy"] = map[string]any{} },
			"indexed":       func(s map[string]any) { s["completionMode"] = "Indexed" },
			"suspended":     func(s map[string]any) { s["suspend"] = true },
			"more parallel": func(s map[string]any) { s["parallelism"] = 5 },
			"unknown":       func(s map[string]any) { s["somethingNew"] = 1 },
		} {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) { mutate(sub(d, "spec")) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("%s: observe = %v", name, err)
			}
		}
	})

	t.Run("the workspace claim and service objects are verified too", func(t *testing.T) {
		for name, mutate := range map[string]func(spec map[string]any){
			"clone from another claim": func(s map[string]any) {
				s["dataSourceRef"] = map[string]any{"kind": "PersistentVolumeClaim", "name": "victim"}
			},
			"selector for a volume": func(s map[string]any) { s["selector"] = map[string]any{"matchLabels": map[string]any{"a": "b"}} },
			"another class":         func(s map[string]any) { s["storageClassName"] = "cheap" },
			"shared access":         func(s map[string]any) { s["accessModes"] = []any{"ReadWriteMany"} },
			"raw block":             func(s map[string]any) { s["volumeMode"] = "Block" },
		} {
			h := newHarness(t)
			o, err := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
			if err != nil {
				t.Fatal(err)
			}
			h.cl.tamper("pvc", testNS, workspaceIdent(h, "run-1").base(), func(d map[string]any) { mutate(sub(d, "spec")) })
			if _, err := h.res.Observe(ctx, o.Handle); !errors.Is(err, ErrUnverified) {
				t.Fatalf("claim %s: observe = %v", name, err)
			}
		}
		for name, mutate := range map[string]func(spec map[string]any){
			"load balancer":      func(s map[string]any) { s["type"] = "LoadBalancer" },
			"external ip":        func(s map[string]any) { s["externalIPs"] = []any{"1.2.3.4"} },
			"selects other pods": func(s map[string]any) { s["selector"] = map[string]any{labelManaged: valueManaged} },
			"external name":      func(s map[string]any) { s["externalName"] = "example.com" },
		} {
			h := newHarness(t)
			spec := h.serviceSpec(scopeA, "run-1", "db", 1)
			o, err := h.res.Ensure(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			id := workspaceIdentFor(h, spec)
			h.cl.setPod(testNS, id.base(), func(p *kubernetes.Pod) {
				p.Status.Phase = "Running"
				p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "service", Ready: true}}
			})
			h.cl.tamper("service", testNS, id.base(), func(d map[string]any) { mutate(sub(d, "spec")) })
			if _, err := h.res.Observe(ctx, o.Handle); !errors.Is(err, ErrUnverified) {
				t.Fatalf("service %s: observe = %v", name, err)
			}
		}
	})

	t.Run("an object without a server document is not adopted", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		job, _ := h.cl.job(testNS, h.jobName(b))
		job.Raw = nil
		rec := h.record(workloadIdent(b))
		if err := h.ad.verifyJob(job, workloadIdent(b), h.target(), rec); !errors.Is(err, ErrUnverified) {
			t.Fatalf("err = %v", err)
		}
	})
}

// Finding 4: every constituent create carries its own marker.
func Test_unit_kube_kernel_runtime_every_create_is_tracked(t *testing.T) {
	ctx := context.Background()
	unknown := &kubernetes.Error{Op: "create", Kind: kubernetes.ErrUnknownOutcome}

	t.Run("an uncertain Service create keeps cleanup open until it settles", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		handle := h.ad.handleOf(id, 1)
		h.cl.failNext("create service", unknown, false)
		_, err := h.res.Ensure(ctx, spec)
		if runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		rec := h.record(id)
		if _, ok := rec.markers[opService]; !ok {
			t.Fatal("the Service create left no marker")
		}
		if _, ok := rec.markers[opPod]; ok || rec.created[opPod] == "" {
			t.Fatalf("the Pod's own operation settled: markers %v created %v", rec.markers, rec.created)
		}
		o, err := h.res.Remove(ctx, handle)
		if err != nil || o.ConfirmedRemoved() || o.State != runtime.ResourceUnknown {
			t.Fatalf("remove before the Service settles = %+v %v", o, err)
		}
		// The delayed create now lands, after Remove returned.
		svc := h.ad.serviceObject(id, *spec.Service, h.target())
		if _, err := h.cl.CreateService(ctx, testNS, svc); err != nil {
			t.Fatal(err)
		}
		o, err = h.res.Remove(ctx, handle)
		if err != nil || o.ConfirmedRemoved() {
			t.Fatalf("a late Service is deleted, but the window is still open: %+v %v", o, err)
		}
		if n := h.cl.names(h.cl.services); len(n) != 0 {
			t.Fatalf("the late Service remains: %v", n)
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("final = %+v %v", o, err)
		}
	})

	t.Run("a later definite outcome does not clear an earlier uncertain create", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		h.cl.failNext("create service", unknown, false)
		_, _ = h.res.Ensure(ctx, spec)
		h.cl.failNext("create service", &kubernetes.Error{Op: "create service", Kind: kubernetes.ErrForbidden}, false)
		_, err := h.res.Ensure(ctx, spec)
		if !errors.Is(err, runtime.ErrUncertain) {
			t.Fatalf("a refusal after the Pod exists is uncertain as a whole: %v", err)
		}
		if _, ok := h.record(id).markers[opService]; !ok {
			t.Fatal("the earlier uncertain create's evidence was erased")
		}
		// A successful retry still leaves the evidence of the earlier one.
		if _, err := h.res.Ensure(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.record(id).markers[opService]; !ok {
			t.Fatal("a successful retry erased the earlier create's evidence")
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.res.Remove(ctx, h.ad.handleOf(id, 1)); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("once the window passes: %+v %v", o, err)
		}
	})

	t.Run("an uncertain step Secret create keeps the workload open", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
		b := binding(scopeA, "run-1", "deploy", 1, "d", 1)
		req := h.startRequest(b, ws)
		req.Spec.Secrets = []runtime.SecretRef{ref}
		req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: "deploy", Directory: "/kodama/secrets", Refs: []runtime.SecretRef{ref}}
		h.cl.failNext("create secret", nil, false) // the fence record's own create passes
		h.cl.failNext("create secret", unknown, false)
		if _, err := h.start(req); runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("a Job was created without its secrets")
		}
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseUnknown {
			t.Fatalf("remove = %+v %v", o, err)
		}
		// The delayed Secret lands; the next Remove deletes it.
		id := workloadIdent(b)
		h.cl.mu.Lock()
		h.cl.secrets[key(testNS, id.materialName())] = kubernetes.Secret{Metadata: kubernetes.ObjectMeta{
			Name: id.materialName(), Namespace: testNS, UID: "late", ResourceVersion: "1", Labels: id.labels(roleMaterial), Annotations: id.annotations()}}
		h.cl.mu.Unlock()
		h.clock.Advance(2 * time.Minute)
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("remove = %+v %v", o, err)
		}
		if _, err := h.cl.GetSecret(ctx, testNS, id.materialName()); err == nil {
			t.Fatal("the late Secret remains")
		}
	})

	t.Run("a Job create that lands after a retry succeeded keeps the identity open", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.cl.failNext("create job", unknown, false)
		_, _ = h.start(h.startRequest(b, ws))
		h.mustStart(h.startRequest(b, ws)) // the retry creates it definitely
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseUnknown {
			t.Fatalf("the first request may still land: %+v %v", o, err)
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("remove = %+v %v", o, err)
		}
	})
}

// Finding 5: workspace release evidence is built from the claim UID and the
// volumes whose claimRef names the claim, and it waits out a binding race.
func Test_unit_kube_kernel_runtime_workspace_release_evidence(t *testing.T) {
	ctx := context.Background()
	retainedPV := func(h *harness, name, uid string, policy string) {
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = policy
		pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: workspaceIdent(h, "run-1").base(), UID: uid}
		h.cl.pvs[name] = pv
	}

	t.Run("a bound claim deleted before any Remove still leaves its retained volume visible", func(t *testing.T) {
		h := newHarness(t)
		o, err := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		if err != nil {
			t.Fatal(err)
		}
		id := workspaceIdent(h, "run-1")
		uid := h.pvcUID(id)
		h.cl.setPVC(testNS, id.base(), func(p *kubernetes.PersistentVolumeClaim) { p.Spec.VolumeName = "pv-ext"; p.Status.Phase = "Bound" })
		retainedPV(h, "pv-ext", uid, "Retain")
		// Someone deletes the claim; the adapter never saw it bound.
		h.dropObject(h.cl.pvcs, id)
		if h.record(id).volume != "" {
			t.Fatal("the test needs a record that never learned the volume")
		}
		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || rm.ConfirmedRemoved() || rm.State != runtime.ResourceFailed || rm.Reason != runtime.ReasonCleanupFailed {
			t.Fatalf("a retained volume found by its claimRef must be reported: %+v %v", rm, err)
		}
		ev, _ := h.res.ReleaseEvidence(ctx, o.Handle)
		if !slices.Equal(ev.Volumes, []string{"pv-ext"}) || !ev.Retained || ev.VolumeGone || ev.Released() {
			t.Fatalf("evidence = %+v", ev)
		}
	})

	t.Run("an unobserved deletion is unconfirmed until a binding race is ruled out", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		id := workspaceIdent(h, "run-1")
		uid := h.pvcUID(id)
		h.dropObject(h.cl.pvcs, id) // deleted while still pending
		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || rm.ConfirmedRemoved() || rm.State != runtime.ResourceStopped {
			t.Fatalf("no volume yet, but the binding is not ruled out: %+v %v", rm, err)
		}
		if ev, _ := h.res.ReleaseEvidence(ctx, o.Handle); !ev.Unconfirmed {
			t.Fatalf("evidence = %+v", ev)
		}
		// The provisioner finishes the volume it was creating for the claim.
		retainedPV(h, "pv-late", uid, "Delete")
		h.clock.Advance(2 * time.Minute)
		rm, err = h.res.Remove(ctx, o.Handle)
		if err != nil || rm.ConfirmedRemoved() || rm.State != runtime.ResourceStopped {
			t.Fatalf("a late volume for the claim keeps the workspace unreleased: %+v %v", rm, err)
		}
		delete(h.cl.pvs, "pv-late")
		if rm, err = h.res.Remove(ctx, o.Handle); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("released = %+v %v", rm, err)
		}
	})

	t.Run("a claim that was never issued has nothing to wait for", func(t *testing.T) {
		h := newHarness(t)
		handle := h.workspaceHandle(scopeA, "run-1")
		rm, err := h.res.Remove(ctx, handle)
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
	})

	t.Run("another claim's volume is not this workspace's", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Retain"
		pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: "someone-else", UID: "u"}
		h.cl.pvs["pv-other"] = pv
		h.clock.Advance(0)
		if _, err := h.res.Remove(ctx, o.Handle); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(2 * time.Minute)
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
	})
}

// Finding 6: Pods whose Job is gone are inspected and stopped.
func Test_unit_kube_kernel_runtime_orphan_pods_without_a_job(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*harness, runtime.RuntimeBinding, kubernetes.Pod) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		pod := h.cl.startPod(testNS, h.jobName(b))
		h.cl.mu.Lock()
		delete(h.cl.jobs, key(testNS, h.jobName(b)))
		h.cl.mu.Unlock()
		return h, b, pod
	}

	t.Run("observation stays running while an orphan Pod runs", func(t *testing.T) {
		h, b, _ := setup(t)
		if o, err := h.rt.Observe(ctx, b); err != nil || o.Phase != runtime.PhaseRunning {
			t.Fatalf("observe = %+v %v", o, err)
		}
		// Even after the workload was stopped, a Pod that still runs is not "exited".
		h.cl.foreground = true
		o, err := h.rt.Stop(ctx, b)
		if err != nil || o.Phase == runtime.PhaseExited || o.Phase == runtime.PhaseAbsent {
			t.Fatalf("stop with a running orphan Pod = %+v %v", o, err)
		}
	})

	t.Run("stop terminates the orphan Pod and only then reports it stopped", func(t *testing.T) {
		h, b, pod := setup(t)
		h.cl.foreground = true
		o, err := h.rt.Stop(ctx, b)
		if err != nil || o.Phase != runtime.PhaseRunning {
			t.Fatalf("a terminating Pod is not yet stopped: %+v %v", o, err)
		}
		if got := h.cl.deletes[len(h.cl.deletes)-1]; got != "pod "+pod.Metadata.Name+" uid="+pod.Metadata.UID {
			t.Fatalf("the Pod was not deleted under its UID: %q", got)
		}
		h.cl.finalize()
		o, err = h.rt.Observe(ctx, b)
		if err != nil || o.Phase != runtime.PhaseExited || o.Result.Code != runtime.ResultCancelled {
			t.Fatalf("after termination: %+v %v", o, err)
		}
	})

	t.Run("a terminated orphan Pod without a Job is unknown, not absent", func(t *testing.T) {
		h, b, pod := setup(t)
		h.cl.setPod(testNS, pod.Metadata.Name, func(p *kubernetes.Pod) { p.Status.Phase = "Succeeded" })
		if o, err := h.rt.Observe(ctx, b); err != nil || o.Phase != runtime.PhaseUnknown {
			t.Fatalf("observe = %+v %v", o, err)
		}
	})
}

// Finding 8: an established object that disappeared is lost, not recreated.
func Test_unit_kube_kernel_runtime_lost_state_is_not_recreated(t *testing.T) {
	ctx := context.Background()

	t.Run("a service Pod", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		if _, err := h.res.Ensure(ctx, spec); err != nil {
			t.Fatal(err)
		}
		h.dropObject(h.cl.pods, id)
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonServiceLost {
			t.Fatalf("ensure after loss = %+v %v", o, err)
		}
		if h.cl.count("create pod") != 1 {
			t.Fatal("a lost service Pod was recreated")
		}
	})

	t.Run("a Service object", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		o, _ := h.res.Ensure(ctx, spec)
		h.cl.setPod(testNS, id.base(), func(p *kubernetes.Pod) {
			p.Status.Phase = "Running"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "service", Ready: true}}
		})
		h.dropObject(h.cl.services, id)
		got, err := h.res.Ensure(ctx, spec)
		if err != nil || got.State != runtime.ResourceFailed || got.Reason != runtime.ReasonServiceLost || h.cl.count("create service") != 1 {
			t.Fatalf("ensure after loss = %+v %v", got, err)
		}
		_ = o
	})

	t.Run("a workspace claim", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		if _, err := h.res.Ensure(ctx, spec); err != nil {
			t.Fatal(err)
		}
		h.dropObject(h.cl.pvcs, workspaceIdent(h, "run-1"))
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonWorkspaceLost {
			t.Fatalf("ensure after loss = %+v %v", o, err)
		}
		if h.cl.count("create pvc") != 1 {
			t.Fatal("a lost workspace was silently replaced by an empty one")
		}
	})

	t.Run("a step Job", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		h.cl.mu.Lock()
		delete(h.cl.jobs, key(testNS, h.jobName(b)))
		h.cl.mu.Unlock()
		o, err := h.start(h.startRequest(b, ws))
		if err != nil || o.Phase == runtime.PhaseRunning || h.cl.count("create job") != 1 {
			t.Fatalf("start after loss = %+v %v creates %d", o, err, h.cl.count("create job"))
		}
	})

	t.Run("a replaced instance is not the established one", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		o, _ := h.res.Ensure(ctx, spec)
		h.cl.mu.Lock()
		p := h.cl.pods[key(testNS, id.base())].(kubernetes.Pod)
		p.Metadata.UID = "another-instance"
		h.cl.pods[key(testNS, id.base())] = p
		h.cl.mu.Unlock()
		if _, err := h.res.Observe(ctx, o.Handle); !errors.Is(err, ErrUnverified) {
			t.Fatalf("observe = %v", err)
		}
	})

	t.Run("a definitely rejected initial create may be retried", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		h.cl.failNext("create pvc", &kubernetes.Error{Op: "create pvc", Kind: kubernetes.ErrForbidden}, false)
		if _, err := h.res.Ensure(ctx, spec); !isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
		if o, err := h.res.Ensure(ctx, spec); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("retry = %+v %v", o, err)
		}
	})
}

// Finding 9: a default-deny policy plus another policy that allows traffic in
// is not isolation.
func Test_unit_kube_kernel_runtime_network_isolation_is_the_effective_policy(t *testing.T) {
	ctx := context.Background()
	m := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	policy := func(name string, sel kubernetes.LabelSelector, types []string, ingress []map[string]any, egress []kubernetes.NetworkEgress) kubernetes.NetworkPolicy {
		return kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: name}, Spec: kubernetes.NetworkPolicySpec{PodSelector: sel, PolicyTypes: types, Ingress: ingress, Egress: egress}}
	}
	both := []string{"Ingress", "Egress"}
	all := kubernetes.LabelSelector{}
	port := func(n float64) []kubernetes.NetworkPort { return []kubernetes.NetworkPort{{Protocol: "TCP", Port: n}} }
	except := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

	admitted := func(t *testing.T, extra ...kubernetes.NetworkPolicy) error {
		h := newHarness(t, func(c *Config) { c.NetworkPolicyEnforced = true })
		h.cl.netpols[testNS] = append([]kubernetes.NetworkPolicy{defaultDeny()}, extra...)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		req := h.startRequest(binding(scopeA, "run-1", "net", 1, "d", 1), ws)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		_, err := h.rt.Start(ctx, req)
		if err != nil && !(errors.Is(err, runtime.ErrCapability) && isNotReached(err)) {
			t.Fatalf("a refusal must be a not-reached capability error: %v", err)
		}
		return err
	}

	refused := map[string]kubernetes.NetworkPolicy{
		"allow-all ingress":                policy("allow-all", all, []string{"Ingress"}, []map[string]any{{}}, nil),
		"ingress from every namespace":     policy("ns", all, []string{"Ingress"}, []map[string]any{{"from": []any{m("namespaceSelector", map[string]any{})}}}, nil),
		"ingress from a CIDR":              policy("cidr", all, []string{"Ingress"}, []map[string]any{{"from": []any{m("ipBlock", m("cidr", "0.0.0.0/0"))}}}, nil),
		"allow-all egress":                 policy("egress", all, []string{"Egress"}, nil, []kubernetes.NetworkEgress{{}}),
		"egress to every address":          policy("anywhere", all, both, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0"}}}}}),
		"public egress on any port":        policy("public", all, both, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0", Except: except}}}}}),
		"public egress without excepts":    policy("noexcept", all, both, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0"}}}, Ports: port(443)}}),
		"other namespaces' pods":           policy("xns", all, both, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{NamespaceSelector: &kubernetes.LabelSelector{}}}, Ports: port(8080)}}),
		"all ports to anywhere":            policy("ports", all, both, nil, []kubernetes.NetworkEgress{{Ports: port(8080)}}),
		"allow-all by the adapter's label": policy("label", kubernetes.LabelSelector{MatchLabels: map[string]string{labelManaged: valueManaged}}, []string{"Ingress"}, []map[string]any{{}}, nil),
		"allow-all by an expression":       policy("expr", kubernetes.LabelSelector{MatchExpressions: []map[string]any{{"key": "x", "operator": "Exists"}}}, []string{"Ingress"}, []map[string]any{{}}, nil),
		"port range":                       policy("range", all, both, nil, []kubernetes.NetworkEgress{{Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(53), EndPort: ptr(int32(9000))}}}}),
	}
	for name, p := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			if err := admitted(t, p); err == nil {
				t.Fatal("default-deny plus this policy was admitted as isolated")
			}
		})
	}

	accepted := map[string]kubernetes.NetworkPolicy{
		"same-namespace ingress":  policy("same", all, []string{"Ingress"}, []map[string]any{{"from": []any{m("podSelector", map[string]any{})}}}, nil),
		"dns anywhere":            policy("dns", all, []string{"Egress"}, nil, []kubernetes.NetworkEgress{{Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(53)}, {Protocol: "TCP", Port: float64(53)}}}}),
		"dns in kube-system":      policy("dns-ns", all, []string{"Egress"}, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}}, Ports: port(53)}}),
		"public web egress":       policy("web", all, both, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0", Except: except}}}, Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(80)}, {Protocol: "TCP", Port: float64(443)}}}}),
		"same-namespace egress":   policy("pods", all, []string{"Egress"}, nil, []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{PodSelector: &kubernetes.LabelSelector{}}}}}),
		"a policy for other pods": policy("others", kubernetes.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, []string{"Ingress"}, []map[string]any{{}}, nil),
	}
	for name, p := range accepted {
		t.Run("accepted: "+name, func(t *testing.T) {
			if err := admitted(t, p); err != nil {
				t.Fatalf("a tenant-scoped policy was refused: %v", err)
			}
		})
	}

	t.Run("a default-deny that itself allows ingress is not a default-deny", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.NetworkPolicyEnforced = true })
		bad := defaultDeny()
		bad.Spec.Ingress = []map[string]any{{}}
		h.cl.netpols[testNS] = []kubernetes.NetworkPolicy{bad}
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		req := h.startRequest(binding(scopeA, "run-1", "net", 1, "d", 1), ws)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		if _, err := h.rt.Start(ctx, req); !errors.Is(err, runtime.ErrCapability) {
			t.Fatalf("err = %v", err)
		}
	})
}
