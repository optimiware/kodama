package gitworker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Test_unit_gitworker_image_references: a digest-pinned OCI reference is accepted
// with or without a registry host and port, in the development registry's form
// included; every reference that is not pinned by a sha256 digest is refused.
func Test_unit_gitworker_image_references(t *testing.T) {
	d := "@sha256:" + strings.Repeat("ab", 32)
	for _, ok := range []string{
		"localhost:33101/t129/scaffold" + d,
		"localhost:5000/scope/kodama" + d,
		"registry.example:5000/org/kodama:1.2.3" + d,
		"registry.example/org/team/kodama" + d,
		"10.0.0.7:5000/kodama" + d,
		"ghcr.io/optimiware/kodama" + d,
		"kodama" + d,
		"library/kodama:latest" + d,
	} {
		s := testSpec()
		s.Image = ok
		if _, err := gitworker.Render(acme, s); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "localhost:33101/t129/scaffold", "localhost:33101/t129/scaffold:dev", "registry.example/kodama:latest",
		"registry.example/kodama@sha256:" + strings.Repeat("a", 63), "registry.example/kodama@sha512:" + strings.Repeat("a", 128),
		"registry.example/Kodama" + d, "registry.example/kodama " + d, "https://registry.example/kodama" + d,
		"registry.example:/kodama" + d, "registry.example/kodama@sha256:" + strings.Repeat("A", 64), "/kodama" + d,
		"registry.example/kodama" + d + "x", strings.Repeat("a", 600) + d,
	} {
		s := testSpec()
		s.Image = bad
		if _, err := gitworker.Render(acme, s); !isErr(err, gitworker.ErrInvalidSpec) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func container0(doc map[string]any) map[string]any {
	return podSpec(doc)["containers"].([]any)[0].(map[string]any)
}

// Test_unit_gitworker_deployment_document_is_exact: a live Deployment must equal
// its rendering as a document. A member that was not rendered is drift unless it
// is a default the API server adds; the server's own spelling of a quantity is not.
func Test_unit_gitworker_deployment_document_is_exact(t *testing.T) {
	ns := nsOf(acme)
	tamper := map[string]func(doc map[string]any){
		"a subPath on the data mount": func(doc map[string]any) {
			container0(doc)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "other-tenant"
		},
		"a subPathExpr": func(doc map[string]any) {
			container0(doc)["volumeMounts"].([]any)[0].(map[string]any)["subPathExpr"] = "$(POD_NAME)"
		},
		"a mountPropagation": func(doc map[string]any) {
			container0(doc)["volumeMounts"].([]any)[1].(map[string]any)["mountPropagation"] = "Bidirectional"
		},
		"a recursive read-only mode": func(doc map[string]any) {
			container0(doc)["volumeMounts"].([]any)[2].(map[string]any)["recursiveReadOnly"] = "Enabled"
		},
		"an env secret reference": func(doc map[string]any) {
			env := container0(doc)["env"].([]any)
			env[0].(map[string]any)["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": "platform-db", "key": "url"}}
			delete(env[0].(map[string]any), "value")
		},
		"an extra env variable": func(doc map[string]any) {
			container0(doc)["env"] = append(container0(doc)["env"].([]any), map[string]any{"name": "DATABASE_URL", "value": "x"})
		},
		"an extra container": func(doc map[string]any) {
			podSpec(doc)["containers"] = append(podSpec(doc)["containers"].([]any), map[string]any{"name": "sidecar", "image": "x"})
		},
		"an init container": func(doc map[string]any) {
			podSpec(doc)["initContainers"] = []any{map[string]any{"name": "init", "image": "x"}}
		},
		"an extra projected volume": func(doc map[string]any) {
			podSpec(doc)["volumes"] = append(podSpec(doc)["volumes"].([]any), map[string]any{"name": "p", "projected": map[string]any{"sources": []any{}}})
		},
		"another claim on the data volume": func(doc map[string]any) {
			podSpec(doc)["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "other"
		},
		"a lifecycle hook": func(doc map[string]any) {
			container0(doc)["lifecycle"] = map[string]any{"postStart": map[string]any{"exec": map[string]any{"command": []any{"x"}}}}
		},
		"an ephemeral port on the host": func(doc map[string]any) {
			container0(doc)["ports"].([]any)[0].(map[string]any)["hostPort"] = 8443
		},
		"a container resize policy": func(doc map[string]any) {
			container0(doc)["resizePolicy"] = []any{map[string]any{"resourceName": "cpu", "restartPolicy": "NotRequired"}}
		},
		"host aliases":      func(doc map[string]any) { podSpec(doc)["hostAliases"] = []any{map[string]any{"ip": "1.2.3.4"}} },
		"a toleration":      func(doc map[string]any) { podSpec(doc)["tolerations"] = []any{map[string]any{"operator": "Exists"}} },
		"a priority class":  func(doc map[string]any) { podSpec(doc)["priorityClassName"] = "system-node-critical" },
		"a paused rollout":  func(doc map[string]any) { doc["spec"].(map[string]any)["paused"] = true },
		"min ready seconds": func(doc map[string]any) { doc["spec"].(map[string]any)["minReadySeconds"] = 30 },
		"template annotations": func(doc map[string]any) {
			doc["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"] = map[string]any{"x": "y"}
		},
		"a changed probe":      func(doc map[string]any) { container0(doc)["readinessProbe"].(map[string]any)["periodSeconds"] = 3600 },
		"a dropped capability": func(doc map[string]any) { delete(container0(doc)["securityContext"].(map[string]any), "capabilities") },
		"an added capability": func(doc map[string]any) {
			container0(doc)["securityContext"].(map[string]any)["capabilities"] = map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_ADMIN"}}
		},
		"an AppArmor profile": func(doc map[string]any) {
			container0(doc)["securityContext"].(map[string]any)["appArmorProfile"] = map[string]any{"type": "Unconfined"}
		},
		"a procMount": func(doc map[string]any) {
			container0(doc)["securityContext"].(map[string]any)["procMount"] = "Unmasked"
		},
		"a user namespace": func(doc map[string]any) { podSpec(doc)["hostUsers"] = true },
	}
	for name, edit := range tamper {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.reconcile(acme)
			before, _ := e.f.deployment(ns)
			e.f.editDeployment(ns, edit)
			e.f.ResetCalls()
			st := e.reconcile(acme)
			if got := e.f.Mutations(); len(got) != 1 || got[0] != "replace deployment" {
				t.Fatalf("mutations %v", got)
			}
			if len(st.Drift) == 0 {
				t.Fatal("the drift was not reported")
			}
			for _, d := range st.Drift {
				if strings.Contains(d, "NET_ADMIN") || strings.Contains(d, "platform-db") || strings.Contains(d, "other-tenant") {
					t.Fatalf("a value leaked into the report: %s", d)
				}
			}
			after, _ := e.f.deployment(ns)
			if after.Metadata.UID != before.Metadata.UID {
				t.Fatal("the Deployment was recreated")
			}
			e.f.ResetCalls()
			if again := e.reconcile(acme); len(again.Actions) != 0 || len(again.Drift) != 0 {
				t.Fatalf("the corrected Deployment still differs: %+v", again)
			}
		})
	}

	t.Run("server spellings of quantities are not drift", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.editDeployment(ns, func(doc map[string]any) {
			r := container0(doc)["resources"].(map[string]any)
			r["limits"].(map[string]any)["cpu"] = "1000m"
			r["limits"].(map[string]any)["memory"] = "536870912"
			r["requests"].(map[string]any)["cpu"] = "0.1"
		})
		e.f.ResetCalls()
		if st := e.reconcile(acme); len(st.Actions) != 0 || len(e.f.Mutations()) != 0 {
			t.Fatalf("%+v %v", st, e.f.Mutations())
		}
	})

	t.Run("the server's own defaults are not drift", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.ResetCalls()
		if st := e.reconcile(acme); len(st.Actions) != 0 || len(st.Drift) != 0 {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("a Service and a policy are documents too", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.mu.Lock()
		k := key(ns, gitworker.PolicyIngress)
		p := e.f.pols[k]
		p.Spec.PolicyTypes = []string{"Ingress", "Egress"}
		e.f.pols[k] = p
		e.f.mu.Unlock()
		e.f.ResetCalls()
		st := e.reconcile(acme)
		if got := e.f.Mutations(); len(got) != 1 || got[0] != "replace networkpolicy" || len(st.Drift) == 0 {
			t.Fatalf("%v %+v", got, st)
		}
	})
}

// Test_unit_gitworker_readiness_invalidated_on_unsafe_evidence: a cached ready
// observation survives only failures that establish nothing; once drift was found
// or a mutation began, any failure of the pass withdraws it.
func Test_unit_gitworker_readiness_invalidated_on_unsafe_evidence(t *testing.T) {
	ns := nsOf(acme)
	ctx := context.Background()
	privileged := func(doc map[string]any) {
		container0(doc)["securityContext"].(map[string]any)["privileged"] = true
	}
	withdrawn := func(t *testing.T, e *env) {
		t.Helper()
		if _, err := e.rec.Endpoint(ctx, acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("a worker with unsafe evidence still resolves: %v", err)
		}
		if st, _ := e.rec.Status(acme); st.Ready {
			t.Fatal("the status is still ready")
		}
	}

	for name, failure := range map[string]error{
		"a conflict":         &kubernetes.Error{Op: "replace deployment", Kind: kubernetes.ErrConflict},
		"an unknown outcome": &kubernetes.Error{Op: "replace deployment", Kind: kubernetes.ErrUnknownOutcome},
		"an outage":          &kubernetes.Error{Op: "replace deployment", Kind: kubernetes.ErrUnavailable},
	} {
		t.Run("a privileged Deployment whose correction fails with "+name, func(t *testing.T) {
			e := newEnv(t)
			e.reconcile(acme)
			if _, err := e.rec.Endpoint(ctx, acme); err != nil {
				t.Fatal(err)
			}
			e.f.editDeployment(ns, privileged)
			e.f.fail["replace deployment"] = failure
			_, err := e.try(acme)
			if !isErr(err, gitworker.ErrUnavailable) {
				t.Fatalf("%v", err)
			}
			withdrawn(t, e)
		})
	}

	t.Run("a lost replace that may have applied", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.spec.Image = testImage2
		e.f.lose["replace deployment"] = true
		if _, err := e.try(acme); !isErr(err, kubernetes.ErrUnknownOutcome) && !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		withdrawn(t, e)
	})

	t.Run("a failed create of any kind", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.mu.Lock()
		delete(e.f.svcs, key(ns, gitworker.ServiceName))
		e.f.mu.Unlock()
		e.f.fail["create service"] = &kubernetes.Error{Op: "create service", Kind: kubernetes.ErrUnavailable}
		if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		withdrawn(t, e)
	})

	t.Run("a read that fails after the correction", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.editDeployment(ns, privileged)
		e.f.ResetCalls()
		e.f.failNth["list pods"] = 2 // the isolation check read them; the observation after the correction fails
		if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		withdrawn(t, e)
	})

	t.Run("a namespace that is going away", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.mu.Lock()
		n := e.f.nss[ns]
		n.Metadata.DeletionTimestamp = "2026-10-30T12:00:00Z"
		e.f.nss[ns] = n
		e.f.mu.Unlock()
		if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		withdrawn(t, e)
	})

	t.Run("pure observation failures keep a fresh ready status", func(t *testing.T) {
		for _, op := range []string{"get namespace", "get storageclass", "list networkpolicies", "list pods", "get deployment", "get pvc"} {
			e := newEnv(t)
			e.reconcile(acme)
			e.f.fail[op] = &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}
			if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
				t.Fatalf("%s: %v", op, err)
			}
			if _, err := e.rec.Endpoint(ctx, acme); err != nil {
				t.Fatalf("%s: a failed read withdrew a fresh observation: %v", op, err)
			}
		}
		e := newEnv(t)
		e.reconcile(acme)
		e.id.err = errors.New("down")
		_, _ = e.try(acme)
		if _, err := e.rec.Endpoint(ctx, acme); err != nil {
			t.Fatalf("an identity outage withdrew it: %v", err)
		}
	})
}

// Test_unit_gitworker_isolation_before_delivery: a namespace left with a foreign
// policy or Pod by a partial provisioning receives neither the worker's identity
// nor a worker, and provisioning resumes once the foreign object is gone.
func Test_unit_gitworker_isolation_before_delivery(t *testing.T) {
	ns := nsOf(acme)
	allowAll := kubernetes.NetworkPolicy{
		Metadata: kubernetes.ObjectMeta{Name: "allow-all"},
		Spec: kubernetes.NetworkPolicySpec{
			PodSelector: kubernetes.LabelSelector{}, PolicyTypes: []string{"Ingress"},
			Ingress: []map[string]any{{"from": []map[string]any{{"namespaceSelector": map[string]any{}}}}},
		},
	}
	// partial leaves the namespace created by an interrupted first pass.
	partial := func(t *testing.T) *env {
		e := newEnv(t)
		e.f.fail["create serviceaccount"] = &kubernetes.Error{Op: "create serviceaccount", Kind: kubernetes.ErrUnavailable}
		if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		delete(e.f.fail, "create serviceaccount")
		if _, ok := e.f.namespace(ns); !ok {
			t.Fatal("no partial namespace")
		}
		e.f.ResetCalls()
		return e
	}
	none := func(t *testing.T, e *env) {
		t.Helper()
		for _, op := range []string{"create secret", "update secret", "create deployment", "replace deployment", "create pvc", "create service", "create serviceaccount", "create networkpolicy"} {
			if e.f.Calls(op) != 0 {
				t.Fatalf("%s ran in an unisolated namespace", op)
			}
		}
	}

	t.Run("an allow-all policy", func(t *testing.T) {
		e := partial(t)
		e.f.putPolicy(ns, allowAll)
		st, err := e.try(acme)
		if !isErr(err, gitworker.ErrIsolation) || st.Ready {
			t.Fatalf("%+v %v", st, err)
		}
		none(t, e)
		if _, ok := e.f.secret(ns); ok {
			t.Fatal("the identity was delivered")
		}
		if _, ok := e.f.deployment(ns); ok {
			t.Fatal("a worker was created")
		}
		// The foreign policy is removed by an operator; provisioning resumes.
		e.f.deletePolicy(ns, "allow-all")
		if st := e.reconcile(acme); !st.Ready {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("a stray Pod", func(t *testing.T) {
		e := partial(t)
		e.f.putPod(ns, kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "stray"}})
		if _, err := e.try(acme); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("%v", err)
		}
		none(t, e)
		e.f.deletePod(ns, "stray")
		if st := e.reconcile(acme); !st.Ready {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("a policy that appears beside a running worker blocks a replacement and a token rotation", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.putPolicy(ns, allowAll)
		e.spec.Image = testImage2
		e.id.token = "rotated"
		e.f.ResetCalls()
		if _, err := e.try(acme); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("%v", err)
		}
		if got := e.f.Mutations(); len(got) != 0 {
			t.Fatalf("the namespace was changed while it was not isolated: %v", got)
		}
		if _, err := e.rec.Endpoint(context.Background(), acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a stray Pod beside an existing worker", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.putPod(ns, kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "stray"}})
		e.spec.Image = testImage2
		e.f.ResetCalls()
		if _, err := e.try(acme); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("%v", err)
		}
		if got := e.f.Mutations(); len(got) != 0 {
			t.Fatalf("%v", got)
		}
	})
}
