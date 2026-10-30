package gitworker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func isErr(err, target error) bool { return errors.Is(err, target) }

const acme = domain.TenantID("t_acme")

func nsOf(t domain.TenantID) string { return gitworker.NamespaceOf(t) }

// Test_unit_gitworker_single_pod_volume (T-129, unit_gitworker_single_pod_volume,
// GR-04): the tenant's volume is one ReadWriteOncePod claim mounted by the one
// worker Pod of a single-replica Recreate Deployment; a class that cannot give
// single-Pod access is refused before anything is created; a second Pod in the
// namespace makes the worker not ready.
func Test_unit_gitworker_single_pod_volume(t *testing.T) {
	t.Run("one claim, one writer", func(t *testing.T) {
		o := mustRender(t, acme, testSpec())
		c := o.Claim
		if len(c.Spec.AccessModes) != 1 || c.Spec.AccessModes[0] != "ReadWriteOncePod" {
			t.Fatalf("access modes %v", c.Spec.AccessModes)
		}
		if c.Spec.StorageClassName == nil || *c.Spec.StorageClassName != "fast" {
			t.Fatal("the claim does not name the class (the cluster default is never used)")
		}
		if len(c.Metadata.OwnerReferences) != 0 {
			t.Fatal("the claim has an owner: deleting the owner would delete the volume")
		}
		d := o.Deployment
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != "Recreate" {
			t.Fatalf("replicas %v strategy %q", d.Spec.Replicas, d.Spec.Strategy.Type)
		}
		pod := d.Spec.Template.Spec
		if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
			t.Fatal("the worker Pod must have exactly one container")
		}
		var claims []string
		for _, v := range pod.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims = append(claims, v.PersistentVolumeClaim.ClaimName)
				if v.PersistentVolumeClaim.ReadOnly {
					t.Fatal("the worker's volume is read-only")
				}
			}
		}
		if len(claims) != 1 || claims[0] != gitworker.ClaimName {
			t.Fatalf("claims mounted: %v", claims)
		}
		mounts := map[string]string{}
		for _, m := range pod.Containers[0].VolumeMounts {
			mounts[m.Name] = m.MountPath
		}
		if mounts["data"] != gitworker.DataRoot {
			t.Fatalf("mounts %v", mounts)
		}
		if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
			t.Fatal("the volume is not group-writable by the worker")
		}
		if marshal(t, o.Service.Spec.Selector) != marshal(t, d.Spec.Selector.MatchLabels) {
			t.Fatal("the Service does not select exactly the Deployment's Pods")
		}
	})

	refusals := map[string]func(*env){
		"a class that does not exist": func(e *env) { e.spec.Storage.Class = "gone" },
		"a class that deletes its volumes": func(e *env) {
			e.f.classes["fast"] = kubernetes.StorageClass{Provisioner: "csi.test", ReclaimPolicy: "Delete"}
		},
		"a class without a reclaim policy": func(e *env) { e.f.classes["fast"] = kubernetes.StorageClass{Provisioner: "csi.test"} },
		"a provisioner not declared RWOP": func(e *env) {
			e.f.classes["fast"] = kubernetes.StorageClass{Provisioner: "kubernetes.io/host-path", ReclaimPolicy: "Retain"}
		},
		"a class without a provisioner": func(e *env) { e.f.classes["fast"] = kubernetes.StorageClass{ReclaimPolicy: "Retain"} },
		"a class that cannot be read (denied)": func(e *env) {
			e.f.fail["get storageclass"] = &kubernetes.Error{Op: "get storageclass", Kind: kubernetes.ErrForbidden}
		},
	}
	for name, setup := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			st, err := e.rec.Reconcile(context.Background(), acme, e.spec)
			if err == nil {
				t.Fatalf("reconciled: %+v", st)
			}
			if !isErr(err, gitworker.ErrStorage) && !isErr(err, gitworker.ErrUnavailable) {
				t.Fatalf("unexpected class: %v", err)
			}
			if m := e.f.Mutations(); len(m) != 0 {
				t.Fatalf("a refused volume left objects behind: %v", m)
			}
			if _, err := e.rec.Endpoint(context.Background(), acme); err == nil {
				t.Fatal("a refused tenant has an endpoint")
			}
		})
	}

	t.Run("a bound volume that would be deleted with its claim", func(t *testing.T) {
		e := newEnv(t)
		e.f.retainPV = false // the cluster provisions a Delete volume despite the class
		_, err := e.rec.Reconcile(context.Background(), acme, e.spec)
		if !isErr(err, gitworker.ErrStorage) {
			t.Fatalf("got %v", err)
		}
		if st, _ := e.rec.Status(acme); st.Ready {
			t.Fatal("a worker on a deletable volume is ready")
		}
	})

	t.Run("a volume that is not the claim's", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.mu.Lock()
		for k, pv := range e.f.pvs {
			pv.Spec.ClaimRef.UID = "someone-else"
			e.f.pvs[k] = pv
		}
		e.f.mu.Unlock()
		if _, err := e.rec.Reconcile(context.Background(), acme, e.spec); !isErr(err, gitworker.ErrStorage) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a second Pod in the namespace is not ready", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		// A Pod that mounts the claim would be refused by ReadWriteOncePod, but it
		// must not be tolerated in the namespace either.
		e.f.putPod(nsOf(acme), kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "second-writer"}})
		st, err := e.rec.Reconcile(context.Background(), acme, e.spec)
		if !isErr(err, gitworker.ErrIsolation) || st.Ready {
			t.Fatalf("ready=%v err=%v", st.Ready, err)
		}
		if _, err := e.rec.Endpoint(context.Background(), acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("endpoint of a tenant with a foreign Pod: %v", err)
		}
	})

	t.Run("a Pod that only claims the Deployment's labels", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		yes := true
		e.f.putPod(nsOf(acme), kubernetes.Pod{Metadata: kubernetes.ObjectMeta{
			Name: "impostor", Labels: map[string]string{gitworker.LabelApp: gitworker.ValueApp},
			OwnerReferences: []kubernetes.OwnerReference{{Kind: "ReplicaSet", Name: "x", UID: "not-ours", Controller: &yes}},
		}})
		if _, err := e.rec.Reconcile(context.Background(), acme, e.spec); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a Pod controlled by a ReplicaSet of another Deployment", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		yes := true
		e.f.mu.Lock()
		e.f.rss[key(nsOf(acme), "other-rs")] = kubernetes.ReplicaSet{Metadata: kubernetes.ObjectMeta{Name: "other-rs", UID: "rs-other",
			OwnerReferences: []kubernetes.OwnerReference{{Kind: "Deployment", Name: gitworker.DeploymentName, UID: "another-deployment", Controller: &yes}}}}
		e.f.mu.Unlock()
		e.f.putPod(nsOf(acme), kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "p2", OwnerReferences: []kubernetes.OwnerReference{{Kind: "ReplicaSet", Name: "other-rs", UID: "rs-other", Controller: &yes}}}})
		if _, err := e.rec.Reconcile(context.Background(), acme, e.spec); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a worker that is starting is converging, not an error", func(t *testing.T) {
		e := newEnv(t)
		e.f.setReady(false)
		st, err := e.rec.Reconcile(context.Background(), acme, e.spec)
		if err != nil || st.Ready || st.Phase != gitworker.PhaseConverging || st.Reason == "" {
			t.Fatalf("%+v %v", st, err)
		}
		e.f.setReady(true)
		// Replacing the Deployment restarts it with a ready Pod.
		e.spec.Image = testImage2
		if st := e.reconcile(acme); !st.Ready {
			t.Fatalf("%+v", st)
		}
	})
}

func tid(s string) domain.TenantID { return domain.TenantID(s) }
