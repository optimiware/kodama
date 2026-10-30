package gitworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func (e *env) try(tenant domain.TenantID) (gitworker.Status, error) {
	e.t.Helper()
	return e.rec.Reconcile(context.Background(), tenant, e.spec)
}

// marks returns metadata that copies every label and annotation of a rendered
// object: all the evidence that can be copied.
func marks(t *testing.T, tenant domain.TenantID, spec gitworker.WorkerSpec, role, name string) kubernetes.ObjectMeta {
	t.Helper()
	o := mustRender(t, tenant, spec)
	var m kubernetes.ObjectMeta
	switch role {
	case gitworker.RoleNamespace:
		m = o.Namespace.Metadata
	case gitworker.RoleDeployment:
		m = o.Deployment.Metadata
	case gitworker.RoleClaim:
		m = o.Claim.Metadata
	case gitworker.RoleServiceAccount:
		m = o.ServiceAccount.Metadata
	case gitworker.RoleService:
		m = o.Service.Metadata
	}
	m.Name = name
	return m
}

// Test_unit_gitworker_adopt_verified_only (T-129, unit_gitworker_adopt_verified_only):
// an object is adopted only when its labels, tenant, namespace UID, owner chain
// and fingerprint verify; every other object holding a reserved name is reported
// and never changed or deleted; an owned object that is stale or tampered with is
// corrected in place under its UID, and what cannot be corrected is reported.
func Test_unit_gitworker_adopt_verified_only(t *testing.T) {
	ns := nsOf(acme)

	t.Run("a foreign namespace with the name is never adopted", func(t *testing.T) {
		for name, meta := range map[string]kubernetes.ObjectMeta{
			"no marks": {Name: ns},
			"copied labels but no fingerprint": {Name: ns, Labels: map[string]string{
				gitworker.LabelOwner: gitworker.ValueOwner, gitworker.LabelTenantKey: gitworker.TenantKey(acme), gitworker.LabelObject: gitworker.RoleNamespace}, Annotations: map[string]string{gitworker.AnnTenant: string(acme)}},
			"another tenant's marks": marksOf(t, "t_other", ns, gitworker.RoleNamespace),
		} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.f.putNamespace(kubernetes.Namespace{Metadata: meta})
				before, _ := e.f.namespace(ns)
				st, err := e.try(acme)
				if !isErr(err, gitworker.ErrForeign) || st.Ready {
					t.Fatalf("ready=%v err=%v", st.Ready, err)
				}
				if m := e.f.Mutations(); len(m) != 0 {
					t.Fatalf("a foreign namespace was written or its tenant populated: %v", m)
				}
				after, _ := e.f.namespace(ns)
				if canonicalJSON(before) != canonicalJSON(after) {
					t.Fatal("the foreign namespace changed")
				}
				if _, err := e.rec.Endpoint(context.Background(), acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
					t.Fatalf("endpoint: %v", err)
				}
			})
		}
	})

	// Each case damages the object of the first reconciliation and reconciles again.
	foreign := map[string]func(e *env){
		"a Deployment without marks": func(e *env) {
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: gitworker.DeploymentName}})
		},
		"a Deployment with every copied label and annotation": func(e *env) {
			// Everything copyable, but no namespace UID.
			m := marks(t, acme, e.spec, gitworker.RoleDeployment, gitworker.DeploymentName)
			delete(m.Annotations, gitworker.AnnNamespaceUID)
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: m})
		},
		"a Deployment from another instance of the namespace": func(e *env) {
			m := marks(t, acme, e.spec, gitworker.RoleDeployment, gitworker.DeploymentName)
			m.Annotations = map[string]string{gitworker.AnnTenant: string(acme), gitworker.AnnFingerprint: m.Annotations[gitworker.AnnFingerprint], gitworker.AnnNamespaceUID: "an-older-namespace"}
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: m})
		},
		"a Deployment of another tenant": func(e *env) {
			m := marks(t, "t_other", e.spec, gitworker.RoleDeployment, gitworker.DeploymentName)
			ns0, _ := e.f.namespace(ns)
			m.Annotations[gitworker.AnnNamespaceUID] = ns0.Metadata.UID
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: m})
		},
		"a Deployment with an owner": func(e *env) {
			ns0, _ := e.f.namespace(ns)
			m := marks(t, acme, e.spec, gitworker.RoleDeployment, gitworker.DeploymentName)
			m.Annotations[gitworker.AnnNamespaceUID] = ns0.Metadata.UID
			m.OwnerReferences = []kubernetes.OwnerReference{{Kind: "Foo", Name: "x", UID: "u"}}
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: m})
		},
		"a Deployment with a foreign finalizer": func(e *env) {
			ns0, _ := e.f.namespace(ns)
			m := marks(t, acme, e.spec, gitworker.RoleDeployment, gitworker.DeploymentName)
			m.Annotations[gitworker.AnnNamespaceUID] = ns0.Metadata.UID
			m.Finalizers = []string{"example.com/hold"}
			e.f.putDeployment(ns, kubernetes.Deployment{Metadata: m})
		},
	}
	for name, damage := range foreign {
		t.Run(name+" is reported and untouched", func(t *testing.T) {
			e := newEnv(t)
			e.reconcile(acme)
			damage(e)
			before, _ := e.f.deployment(ns)
			e.f.ResetCalls()
			st, err := e.try(acme)
			if !isErr(err, gitworker.ErrForeign) || st.Ready {
				t.Fatalf("ready=%v err=%v", st.Ready, err)
			}
			if m := e.f.Mutations(); len(m) != 0 {
				t.Fatalf("a foreign object was touched: %v", m)
			}
			after, _ := e.f.deployment(ns)
			if string(before.Raw) != string(after.Raw) {
				t.Fatal("the foreign object changed")
			}
		})
	}

	t.Run("a foreign object of every other kind", func(t *testing.T) {
		for name, damage := range map[string]func(e *env){
			"claim": func(e *env) {
				e.f.putPVC(ns, kubernetes.PersistentVolumeClaim{Metadata: kubernetes.ObjectMeta{Name: gitworker.ClaimName}})
			},
			"service": func(e *env) {
				e.f.mu.Lock()
				e.f.svcs[key(ns, gitworker.ServiceName)] = []byte(`{"metadata":{"name":"git-worker","uid":"x"},"spec":{"type":"ClusterIP"}}`)
				e.f.mu.Unlock()
			},
			"service account": func(e *env) {
				e.f.mu.Lock()
				e.f.sas[key(ns, gitworker.ServiceAccountName)] = kubernetes.ServiceAccount{Metadata: kubernetes.ObjectMeta{Name: gitworker.ServiceAccountName, UID: "x"}}
				e.f.mu.Unlock()
			},
			"secret": func(e *env) {
				e.f.mu.Lock()
				e.f.secrets[key(ns, gitworker.SecretName)] = kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: gitworker.SecretName, UID: "x"}, Data: map[string][]byte{"token": []byte("theirs")}}
				e.f.mu.Unlock()
			},
			"policy": func(e *env) {
				e.f.putPolicy(ns, kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: gitworker.PolicyIngress}})
			},
		} {
			e := newEnv(t)
			e.reconcile(acme)
			damage(e)
			e.f.ResetCalls()
			if _, err := e.try(acme); !isErr(err, gitworker.ErrForeign) {
				t.Fatalf("%s: %v", name, err)
			}
			if len(e.f.Mutations()) != 0 {
				t.Fatalf("%s: a foreign object was touched: %v", name, e.f.Mutations())
			}
		}
	})

	t.Run("an owned object that cannot be corrected in place is reported", func(t *testing.T) {
		for name, damage := range map[string]func(e *env){
			"a claim of another class": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.ClaimName)
				p := e.f.pvcs[k]
				other := "cheap"
				p.Spec.StorageClassName = &other
				e.f.pvcs[k] = p
				e.f.mu.Unlock()
			},
			"a claim that allows several writers": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.ClaimName)
				p := e.f.pvcs[k]
				p.Spec.AccessModes = []string{"ReadWriteMany"}
				e.f.pvcs[k] = p
				e.f.mu.Unlock()
			},
			"a service account that mounts its token": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.ServiceAccountName)
				s := e.f.sas[k]
				s.AutomountServiceAccountToken = nil
				e.f.sas[k] = s
				e.f.mu.Unlock()
			},
			"a service that exposes external addresses": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.ServiceName)
				var doc map[string]any
				_ = json.Unmarshal(e.f.svcs[k], &doc)
				doc["spec"].(map[string]any)["externalIPs"] = []any{"203.0.113.9"}
				e.f.svcs[k], _ = json.Marshal(doc)
				e.f.mu.Unlock()
			},
			"a service on another port": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.ServiceName)
				var doc map[string]any
				_ = json.Unmarshal(e.f.svcs[k], &doc)
				doc["spec"].(map[string]any)["ports"] = []any{map[string]any{"name": "git", "protocol": "TCP", "port": 22, "targetPort": 22}}
				e.f.svcs[k], _ = json.Marshal(doc)
				e.f.mu.Unlock()
			},
			"an immutable identity secret": func(e *env) {
				e.f.mu.Lock()
				k := key(ns, gitworker.SecretName)
				s := e.f.secrets[k]
				yes := true
				s.Immutable = &yes
				e.f.secrets[k] = s
				e.f.mu.Unlock()
				e.id.token = "rotated"
			},
		} {
			e := newEnv(t)
			e.reconcile(acme)
			damage(e)
			e.f.ResetCalls()
			st, err := e.try(acme)
			if !isErr(err, gitworker.ErrDrift) || st.Ready {
				t.Fatalf("%s: ready=%v err=%v", name, st.Ready, err)
			}
			if m := e.f.Mutations(); len(m) != 0 {
				t.Fatalf("%s: wrote %v", name, m)
			}
		}
	})

	t.Run("a Pod Security label relaxed on our namespace is restored", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.mu.Lock()
		n := e.f.nss[ns]
		n.Metadata.Labels["pod-security.kubernetes.io/enforce"] = "privileged"
		n.Metadata.Labels["someone-elses"] = "kept"
		e.f.nss[ns] = n
		uid := n.Metadata.UID
		e.f.mu.Unlock()
		e.f.ResetCalls()
		e.reconcile(acme)
		got, _ := e.f.namespace(ns)
		if got.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" || got.Metadata.UID != uid || got.Metadata.Labels["someone-elses"] != "kept" || e.f.Calls("replace namespace") != 1 {
			t.Fatalf("%+v", got.Metadata.Labels)
		}
	})

	t.Run("a verified object is adopted without a write", func(t *testing.T) {
		e := newEnv(t)
		first := e.reconcile(acme)
		if len(first.Actions) != 9 {
			t.Fatalf("first pass actions %v", first.Actions)
		}
		before, _ := e.f.deployment(ns)
		// A restarted controller: a new reconciler with no memory.
		rec, err := gitworker.New(gitworker.Dependencies{Cluster: e.f, Identity: e.id, Retention: e.ret}, gitworker.Options{Clock: e.clk.Now, RWOPProvisioners: []string{"csi.test"}})
		if err != nil {
			t.Fatal(err)
		}
		e.f.ResetCalls()
		st, err := rec.Reconcile(context.Background(), acme, e.spec)
		if err != nil || !st.Ready || len(st.Actions) != 0 || len(e.f.Mutations()) != 0 {
			t.Fatalf("%+v %v %v", st, err, e.f.Mutations())
		}
		after, _ := e.f.deployment(ns)
		if after.Metadata.UID != before.Metadata.UID || after.Generation != before.Generation {
			t.Fatal("the adopted Deployment was replaced")
		}
	})

	t.Run("a create whose answer was lost is adopted, not repeated", func(t *testing.T) {
		for _, op := range []string{"namespace", "serviceaccount", "pvc", "secret", "service", "networkpolicy", "deployment"} {
			e := newEnv(t)
			e.f.lose["create "+op] = true
			if st, err := e.try(acme); err != nil || !st.Ready {
				t.Fatalf("%s: %+v %v", op, st, err)
			}
			want := 1
			if op == "networkpolicy" {
				want = 3
			}
			if got := e.f.Calls("create " + op); got != want {
				t.Fatalf("%s created %d times", op, got)
			}
		}
	})

	t.Run("an unknown outcome that left nothing is reported unavailable", func(t *testing.T) {
		e := newEnv(t)
		e.f.fail["create namespace"] = &kubernetes.Error{Op: "create namespace", Kind: kubernetes.ErrUnknownOutcome}
		_, err := e.try(acme)
		if !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a stale owned Deployment is replaced under its UID", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		before, _ := e.f.deployment(ns)
		e.spec.Image = testImage2
		e.f.ResetCalls()
		e.reconcile(acme)
		after, _ := e.f.deployment(ns)
		if got := e.f.Mutations(); len(got) != 1 || got[0] != "replace deployment" {
			t.Fatalf("mutations %v", got)
		}
		if after.Metadata.UID != before.Metadata.UID || after.Generation != before.Generation+1 ||
			after.Spec.Template.Spec.Containers[0].Image != testImage2 {
			t.Fatalf("%+v", after.Metadata)
		}
	})

	t.Run("a Deployment tampered with keeps its fingerprint and is still corrected", func(t *testing.T) {
		tamper := map[string]func(doc map[string]any){
			"a privileged container": func(doc map[string]any) {
				c := podSpec(doc)["containers"].([]any)[0].(map[string]any)
				c["securityContext"].(map[string]any)["privileged"] = true
			},
			"another image": func(doc map[string]any) {
				podSpec(doc)["containers"].([]any)[0].(map[string]any)["image"] = "evil.example/x@sha256:" + strings.Repeat("a", 64)
			},
			"a second replica": func(doc map[string]any) { doc["spec"].(map[string]any)["replicas"] = 2 },
			"a rolling update": func(doc map[string]any) {
				doc["spec"].(map[string]any)["strategy"] = map[string]any{"type": "RollingUpdate"}
			},
			"a mounted service account token": func(doc map[string]any) { podSpec(doc)["automountServiceAccountToken"] = true },
			"host networking":                 func(doc map[string]any) { podSpec(doc)["hostNetwork"] = true },
			"an extra hostPath volume": func(doc map[string]any) {
				podSpec(doc)["volumes"] = append(podSpec(doc)["volumes"].([]any), map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}})
			},
			"envFrom a secret": func(doc map[string]any) {
				podSpec(doc)["containers"].([]any)[0].(map[string]any)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "platform-db"}}}
			},
			"an affinity": func(doc map[string]any) {
				podSpec(doc)["affinity"] = map[string]any{"nodeAffinity": map[string]any{}, "x": 1}
			},
			"a runtime class": func(doc map[string]any) { podSpec(doc)["runtimeClassName"] = "other" },
		}
		for name, edit := range tamper {
			e := newEnv(t)
			e.reconcile(acme)
			before, _ := e.f.deployment(ns)
			e.f.editDeployment(ns, edit)
			e.f.ResetCalls()
			st, err := e.try(acme)
			if err != nil || !st.Ready {
				t.Fatalf("%s: %+v %v", name, st, err)
			}
			if got := e.f.Mutations(); len(got) != 1 || got[0] != "replace deployment" {
				t.Fatalf("%s: mutations %v", name, got)
			}
			after, _ := e.f.deployment(ns)
			if after.Metadata.UID != before.Metadata.UID || canonicalJSON(after.Spec) != canonicalJSON(before.Spec) {
				t.Fatalf("%s: not restored", name)
			}
		}
	})

	t.Run("a replace that races with another writer is not forced", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.spec.Image = testImage2
		e.f.fail["replace deployment"] = &kubernetes.Error{Op: "replace deployment", Kind: kubernetes.ErrConflict}
		_, err := e.try(acme)
		if !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
}

func podSpec(doc map[string]any) map[string]any {
	return doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

// marksOf builds the metadata another tenant's namespace of that name would carry.
func marksOf(t *testing.T, tenant domain.TenantID, name, role string) kubernetes.ObjectMeta {
	t.Helper()
	m := marks(t, tenant, testSpec(), role, name)
	m.Name = name
	return m
}
