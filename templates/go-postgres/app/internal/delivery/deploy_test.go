package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/inventory/internal/delivery"
	"example.invalid/inventory/test/support/clusterfake"
	"example.invalid/inventory/test/support/factoryfake"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	envID   = "env_preview"
	ns      = "acme-preview"
	commit  = "1111111111111111111111111111111111111111"
	digest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo    = "registry.example.invalid/acme/inventory"
)

type images struct {
	have map[string]bool
	err  error
}

func (i images) Digest(_ context.Context, r, ref string) (string, error) {
	if i.err != nil {
		return "", i.err
	}
	if !i.have[r+"@"+ref] {
		return "", delivery.ErrImageMissing
	}
	return ref, nil
}

type rig struct {
	t        *testing.T
	fac      *factoryfake.Server
	cluster  *clusterfake.Cluster
	log      *clusterfake.Timeline
	snapshot delivery.Snapshot
	attempt  string
	secrets  string
	in       delivery.Input
	cx       map[string]any
	rt       *delivery.Runtime
}

func baseSnapshot(kind, namespace string) delivery.Snapshot {
	return delivery.Snapshot{
		Format: delivery.SnapshotFormat, Tenant: tenant, Project: project, Environment: envID, Kind: kind, Target: "kind", Namespace: namespace,
		Config: map[string]string{
			"OIDC_ISSUER": "https://idp.example.invalid", "OIDC_AUDIENCE": "inventory", "ERP_BASE_URL": "https://erp.example.invalid",
			"WORKER_TENANT": "11111111-1111-4111-8111-111111111111", "WORKER_SERVICE_ID": "svc-worker", "DATABASE_RUNTIME_ROLE": "inventory_runtime", "REPLICAS": "1",
			"APP_URL": "http://inventory.preview.example.invalid",
		},
		TTLSeconds: 86400, Kubeconfig: "KUBECONFIG",
		Secrets: []delivery.Binding{
			{Ref: "secret://project/kubeconfig", Version: 3, Name: "KUBECONFIG", Delivery: "file"},
			{Ref: "secret://project/db-runtime", Version: 1, Name: "DATABASE_RUNTIME_URL", Delivery: "file"},
			{Ref: "secret://project/db-migrate", Version: 1, Name: "DATABASE_MIGRATION_URL", Delivery: "file"},
			{Ref: "secret://project/cursor", Version: 1, Name: "CURSOR_KEY", Delivery: "file"},
			{Ref: "secret://project/erp", Version: 2, Name: "ERP_TOKEN", Delivery: "file"},
		},
	}
}

func newRig(t *testing.T, mutate ...func(*rig)) *rig {
	t.Helper()
	log := &clusterfake.Timeline{}
	r := &rig{t: t, log: log, fac: factoryfake.New(t, log), cluster: clusterfake.New(log), attempt: "att_deploy_1", snapshot: baseSnapshot("preview", ns), secrets: t.TempDir()}
	r.install()
	for _, m := range mutate {
		m(r)
	}
	return r
}

// install (re)writes the pinned context, the credentials and the run input.
func (r *rig) install() {
	r.t.Helper()
	d, err := r.snapshot.Digest()
	if err != nil {
		r.t.Fatal(err)
	}
	raw, _ := json.Marshal(r.snapshot)
	var secrets []map[string]any
	for _, b := range r.snapshot.Secrets {
		secrets = append(secrets, map[string]any{"ref": b.Ref, "version": b.Version, "name": b.Name, "delivery": b.Delivery})
		if b.Delivery == "file" {
			_ = os.WriteFile(filepath.Join(r.secrets, b.Name), []byte("value-of-"+b.Name+"\n"), 0o600)
		}
	}
	r.cx = map[string]any{
		"tenantId": r.snapshot.Tenant, "projectId": r.snapshot.Project, "runId": "run_1", "stepId": "deploy", "attemptId": r.attempt, "actionKey": "run_1:deploy",
		"command":   map[string]any{"image": "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		"workspace": map[string]any{"access": "read", "source": map[string]any{"repositoryUrl": "https://git.example.invalid/acme/inventory.git", "commit": commit, "branch": nil, "expectedTip": nil}},
		"inputs":    map[string]any{"image": "art_image_1", "image_digest": digest, "image_repository": repo, "environment_snapshot": string(raw)},
		"secrets":   secrets, "environment": map[string]any{"id": r.snapshot.Environment, "configDigest": d},
		"x-namespace": r.snapshot.Namespace, "x-kind": r.snapshot.Kind,
	}
	r.fac.SetContext(r.attempt, r.cx)
	tok := filepath.Join(r.secrets, "..", "token-"+r.attempt)
	_ = os.WriteFile(tok, []byte("runtime-token-"+r.attempt+"\n"), 0o600)
	rt, err := delivery.NewRuntime(r.fac.URL(), tok, r.attempt)
	if err != nil {
		r.t.Fatal(err)
	}
	rt.Backoff = time.Millisecond
	r.in = delivery.Input{
		Kind: r.snapshot.Kind, SnapshotJSON: string(raw), SecretsDir: r.secrets, Getenv: func(string) string { return "" },
		ClusterFor:       func(string) delivery.Cluster { return r.cluster },
		Images:           images{have: map[string]bool{repo + "@" + digest: true}},
		ReadinessTimeout: 500 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	r.rt = rt
}

func (r *rig) deployer() *delivery.Deployer { return &delivery.Deployer{Factory: r.rt, In: r.in} }

func (r *rig) run() delivery.Result {
	r.t.Helper()
	res, err := r.deployer().Run(context.Background())
	if err != nil {
		r.t.Fatalf("internal error: %v", err)
	}
	return res
}

func (r *rig) events() string { return strings.Join(r.log.Events(), "\n") }

func eventIndex(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func TestDeployHappyPathRecordsBeforeApplyAndObservesReadiness(t *testing.T) {
	r := newRig(t)
	res := r.run()
	if !res.Completed || res.DeploymentID == "" {
		t.Fatalf("%+v\n%s", res, r.events())
	}
	ev := r.log.Events()
	reg, applying, apply, succeeded := eventIndex(ev, "factory:register"), eventIndex(ev, "factory:applying"), eventIndex(ev, "cluster:apply:"), eventIndex(ev, "factory:succeeded")
	if !(reg >= 0 && reg < applying && applying < apply && apply < succeeded) {
		t.Fatalf("the record and apply intent must precede the apply, and success must follow it:\n%s", r.events())
	}
	rec, _ := r.fac.Record("run_1:deploy")
	if rec.Status != "succeeded" || rec.Reason != "ready" || len(rec.Objects) != 7 {
		t.Fatalf("%+v", rec)
	}
	for _, o := range rec.Objects {
		if o.UID == "" || o.Ready == nil || !*o.Ready || o.Namespace != ns {
			t.Errorf("structured readiness needs the observed uid and ready flag in the environment namespace: %+v", o)
		}
		ann, uid, ok := r.cluster.Object(delivery.ObjectRef{APIVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name})
		if !ok || uid != o.UID || ann[delivery.AnnotationDeploymentID] != res.DeploymentID {
			t.Errorf("object %s/%s lacks the receipt annotations", o.Kind, o.Name)
		}
	}
	if res.AppURL != "http://inventory.preview.example.invalid" {
		t.Errorf("app url %q", res.AppURL)
	}
	// A repeated run observes the settled record and applies nothing.
	before := r.cluster.ApplyCalls
	if again := r.run(); !again.Completed || r.cluster.ApplyCalls != before {
		t.Fatalf("a settled deployment must not be applied again: %+v applies %d->%d", again, before, r.cluster.ApplyCalls)
	}
}

func TestDeployRefusesMismatchedInputsBeforeAnyEffect(t *testing.T) {
	cases := map[string]func(*rig){
		"digest not covering the content": func(r *rig) {
			r.snapshot.Config["REPLICAS"] = "3" // edited after pinning
			raw, _ := json.Marshal(r.snapshot)
			r.in.SnapshotJSON = string(raw)
		},
		"another tenant": func(r *rig) {
			r.snapshot.Tenant = "t_other"
			r.install()
			r.cx["tenantId"] = tenant
			r.fac.SetContext(r.attempt, r.cx)
		},
		"another environment": func(r *rig) {
			r.snapshot.Environment = "env_other"
			r.install()
			r.cx["environment"] = map[string]any{"id": envID, "configDigest": r.cx["environment"].(map[string]any)["configDigest"]}
			r.fac.SetContext(r.attempt, r.cx)
		},
		"a changed credential binding": func(r *rig) {
			r.install()
			secrets := r.cx["secrets"].([]map[string]any)
			secrets[4]["version"] = int64(9) // rotated after the snapshot was supplied
			r.fac.SetContext(r.attempt, r.cx)
		},
		"stage snapshot in a preview script": func(r *rig) {
			r.snapshot = baseSnapshot("stage", "acme-stage")
			r.install()
			r.in.Kind = "preview"
		},
		"invalid namespace": func(r *rig) { r.snapshot.Namespace = "Not_A_Namespace"; r.install() },
		"unknown snapshot field": func(r *rig) {
			r.install()
			r.in.SnapshotJSON = strings.Replace(r.in.SnapshotJSON, `"format"`, `"extra":1,"format"`, 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, mutate)
			res := r.run()
			if res.Completed || res.Reason != delivery.ReasonSnapshot {
				t.Fatalf("want %s, got %+v", delivery.ReasonSnapshot, res)
			}
			if r.fac.Calls["register"] != 0 || r.cluster.ApplyCalls != 0 || r.cluster.Count() != 0 {
				t.Fatalf("nothing may be registered or applied:\n%s", r.events())
			}
		})
	}
}

func TestDeployRefusesIncompleteInputs(t *testing.T) {
	t.Run("missing explicit configuration", func(t *testing.T) {
		r := newRig(t, func(r *rig) { delete(r.snapshot.Config, "OIDC_ISSUER"); r.install() })
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonConfig || r.fac.Calls["register"] != 0 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("credential not delivered", func(t *testing.T) {
		r := newRig(t)
		_ = os.Remove(filepath.Join(r.secrets, "ERP_TOKEN"))
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonConfig || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("image not in the registry", func(t *testing.T) {
		r := newRig(t)
		r.in.Images = images{have: map[string]bool{}}
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonImage || r.fac.Calls["register"] != 0 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("image not among the pinned inputs", func(t *testing.T) {
		r := newRig(t)
		delete(r.cx["inputs"].(map[string]any), "image")
		r.fac.SetContext(r.attempt, r.cx)
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonImage {
			t.Fatalf("%+v", res)
		}
	})
}

func TestDeployRecovery(t *testing.T) {
	t.Run("a lost registration response observes the same record", func(t *testing.T) {
		r := newRig(t)
		r.fac.LoseResponse("register", 1)
		res := r.run()
		if !res.Completed || r.cluster.Effects != 1 {
			t.Fatalf("%+v\n%s", res, r.events())
		}
		if r.fac.Calls["register"] != 2 {
			t.Fatalf("the identical registration is repeated: %d", r.fac.Calls["register"])
		}
	})

	t.Run("an unreachable factory stops before any effect", func(t *testing.T) {
		r := newRig(t)
		r.fac.Unavailable("register", 100)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonCallback || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("a lost apply-intent response is repeated, not skipped", func(t *testing.T) {
		r := newRig(t)
		r.fac.LoseResponse("applying", 1)
		if res := r.run(); !res.Completed || r.cluster.ApplyCalls != 1 {
			t.Fatalf("%+v applies=%d", res, r.cluster.ApplyCalls)
		}
	})

	t.Run("an unreachable factory at the apply intent means no apply", func(t *testing.T) {
		r := newRig(t)
		r.fac.Unavailable("applying", 100)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonCallback || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v applies=%d", res, r.cluster.ApplyCalls)
		}
	})

	t.Run("a lost apply response is settled by observation without a second apply", func(t *testing.T) {
		r := newRig(t)
		r.cluster.FailAfterEffect = 1
		res := r.run()
		if !res.Completed {
			t.Fatalf("%+v\n%s", res, r.events())
		}
		if r.cluster.ApplyCalls != 1 {
			t.Fatalf("an unknown outcome must be observed, never blindly reapplied: %d applies", r.cluster.ApplyCalls)
		}
		rec, _ := r.fac.Record("run_1:deploy")
		if rec.Status != "succeeded" {
			t.Fatalf("%+v", rec)
		}
		if eventIndex(r.log.Events(), "factory:unknown") < 0 {
			t.Fatalf("the uncertainty is reported to the factory:\n%s", r.events())
		}
	})

	t.Run("an apply that never took effect settles as failed, not reapplied", func(t *testing.T) {
		r := newRig(t)
		r.cluster.FailBeforeEffect = 1
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonApply || r.cluster.ApplyCalls != 1 {
			t.Fatalf("%+v applies=%d", res, r.cluster.ApplyCalls)
		}
		if rec, _ := r.fac.Record("run_1:deploy"); rec.Status != "failed" || rec.Reason != delivery.ReasonApply {
			t.Fatalf("%+v", rec)
		}
	})

	t.Run("a lost success response is reconciled by a retry attempt without disturbing objects", func(t *testing.T) {
		r := newRig(t)
		r.fac.Unavailable("succeeded", 100)
		first := r.run()
		if first.Completed || first.Reason != delivery.ReasonUnreported {
			t.Fatalf("%+v", first)
		}
		rec, _ := r.fac.Record("run_1:deploy")
		if rec.Status != "running" {
			t.Fatalf("the record is not settled yet: %+v", rec)
		}
		r.fac.Unavailable("succeeded", 0)
		second := r.run()
		if !second.Completed {
			t.Fatalf("%+v\n%s", second, r.events())
		}
		if r.cluster.Effects != 1 {
			t.Fatalf("re-applying identical manifests changes nothing: %d effects", r.cluster.Effects)
		}
		if rec, _ := r.fac.Record("run_1:deploy"); rec.Status != "succeeded" {
			t.Fatalf("%+v", rec)
		}
	})

	t.Run("a recorded unknown outcome is observed and settled, never reapplied", func(t *testing.T) {
		for name, present := range map[string]bool{"objects exist": true, "objects absent": false} {
			t.Run(name, func(t *testing.T) {
				r := newRig(t)
				if present {
					// A previous attempt applied and then lost its response.
					r.cluster.FailAfterEffect = 1
					r.fac.Unavailable("unknown", 100)
					r.fac.Unavailable("succeeded", 100)
					_ = r.run()
					r.fac.Unavailable("unknown", 0)
					r.fac.Unavailable("succeeded", 0)
				}
				id := factoryfake.DeploymentID(tenant, project, "run_1:deploy")
				planted := delivery.Record{ID: id, EnvironmentID: envID, Namespace: ns, ActionKey: "run_1:deploy", Status: "running", Reason: "outcome_unknown", OutcomeUnknown: true,
					Source: delivery.Source{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: commit}, Image: delivery.Image{ArtifactID: "art_image_1", Digest: digest}}
				d, _ := r.snapshot.Digest()
				planted.ConfigurationDigest = d
				r.fac.Plant("run_1:deploy", planted, map[string]string{delivery.AnnotationDeploymentID: id, delivery.AnnotationActionKey: "run_1:deploy", delivery.AnnotationConfigDigest: d})
				applies := r.cluster.ApplyCalls
				res := r.run()
				if r.cluster.ApplyCalls != applies {
					t.Fatalf("an unknown outcome must not be reapplied")
				}
				if present && !res.Completed {
					t.Fatalf("observed objects settle as succeeded: %+v\n%s", res, r.events())
				}
				if !present && (res.Completed || res.Reason != delivery.ReasonApply) {
					t.Fatalf("absent objects settle as failed: %+v", res)
				}
			})
		}
	})

	t.Run("an unobservable cluster leaves the unknown outcome unsettled", func(t *testing.T) {
		r := newRig(t)
		id := factoryfake.DeploymentID(tenant, project, "run_1:deploy")
		d, _ := r.snapshot.Digest()
		r.fac.Plant("run_1:deploy", delivery.Record{ID: id, EnvironmentID: envID, ConfigurationDigest: d, Namespace: ns, ActionKey: "run_1:deploy", Status: "running", Reason: "outcome_unknown", OutcomeUnknown: true,
			Source: delivery.Source{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: commit}, Image: delivery.Image{ArtifactID: "art_image_1", Digest: digest}},
			map[string]string{delivery.AnnotationDeploymentID: id, delivery.AnnotationActionKey: "run_1:deploy", delivery.AnnotationConfigDigest: d})
		r.cluster.GetError = errors.New("kubectl get failed: connection refused")
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonObservation {
			t.Fatalf("%+v", res)
		}
		if rec, _ := r.fac.Record("run_1:deploy"); rec.Status != "running" || !rec.OutcomeUnknown {
			t.Fatalf("nothing may be claimed without observation: %+v", rec)
		}
	})

	t.Run("a refused apply while the outcome is unknown settles by observation", func(t *testing.T) {
		r := newRig(t)
		r.cluster.FailAfterEffect = 1
		r.fac.Unavailable("succeeded", 100)
		_ = r.run() // applied, unknown reported, success unacknowledged
		rec, _ := r.fac.Record("run_1:deploy")
		if rec.Reason != "outcome_unknown" {
			t.Fatalf("%+v", rec)
		}
		r.fac.Unavailable("succeeded", 0)
		applies := r.cluster.ApplyCalls
		if res := r.run(); !res.Completed || r.cluster.ApplyCalls != applies {
			t.Fatalf("%+v applies %d -> %d", res, applies, r.cluster.ApplyCalls)
		}
	})
}

func TestDeployForeignAndMismatchedObjects(t *testing.T) {
	t.Run("an object owned by another deployment is never adopted", func(t *testing.T) {
		for name, ann := range map[string]map[string]string{
			"other deployment id": {delivery.AnnotationDeploymentID: "dep_" + strings.Repeat("0", 32)},
			"no annotations":      nil,
		} {
			t.Run(name, func(t *testing.T) {
				r := newRig(t)
				r.cluster.Preexist(delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "inventory-api"}, ann)
				res := r.run()
				if res.Completed || res.Reason != delivery.ReasonForeign || r.cluster.ApplyCalls != 0 {
					t.Fatalf("%+v applies=%d", res, r.cluster.ApplyCalls)
				}
				if got, _, _ := r.cluster.Object(delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "inventory-api"}); got[delivery.AnnotationDeploymentID] == res.DeploymentID {
					t.Fatal("the foreign object was modified")
				}
			})
		}
	})

	t.Run("the credential does not cover the namespace", func(t *testing.T) {
		r := newRig(t)
		r.cluster.Allowed = map[string]bool{"some-other-namespace": true}
		res := r.run()
		if res.Completed || r.cluster.Count() != 0 {
			t.Fatalf("%+v", res)
		}
		if rec, _ := r.fac.Record("run_1:deploy"); rec.Status != "failed" {
			t.Fatalf("a refused apply with nothing observed is a settled failure: %+v", rec)
		}
	})

	t.Run("a receipt for another namespace stops everything", func(t *testing.T) {
		r := newRig(t)
		r.cx["x-namespace"] = "another-namespace"
		r.fac.SetContext(r.attempt, r.cx)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonReceipt || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("a registration that contradicts the recorded action conflicts", func(t *testing.T) {
		r := newRig(t)
		id := factoryfake.DeploymentID(tenant, project, "run_1:deploy")
		d, _ := r.snapshot.Digest()
		r.fac.Plant("run_1:deploy", delivery.Record{ID: id, EnvironmentID: envID, ConfigurationDigest: d, Namespace: ns, ActionKey: "run_1:deploy", Status: "pending", Reason: "registered",
			Source: delivery.Source{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: strings.Repeat("2", 40)}, Image: delivery.Image{ArtifactID: "art_image_1", Digest: digest}},
			map[string]string{delivery.AnnotationDeploymentID: id})
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonIntent || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("a settled failure is not retried into an apply", func(t *testing.T) {
		r := newRig(t)
		r.cluster.FailBeforeEffect = 1
		_ = r.run()
		before := r.cluster.ApplyCalls
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonEarlierFailure || r.cluster.ApplyCalls != before {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("a recorded success whose objects vanished is not silently repaired", func(t *testing.T) {
		r := newRig(t)
		if res := r.run(); !res.Completed {
			t.Fatal(res)
		}
		fresh := clusterfake.New(r.log)
		r.cluster = fresh
		r.in.ClusterFor = func(string) delivery.Cluster { return fresh }
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonNotLive || fresh.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})
}

func TestDeployReadinessAndRevocation(t *testing.T) {
	t.Run("readiness is structured and bounded", func(t *testing.T) {
		r := newRig(t)
		r.cluster.NeverReady = true
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonRollout {
			t.Fatalf("%+v", res)
		}
		rec, _ := r.fac.Record("run_1:deploy")
		if rec.Status != "failed" || rec.Reason != delivery.ReasonRollout {
			t.Fatalf("%+v", rec)
		}
	})

	t.Run("a failed migration job fails the deployment", func(t *testing.T) {
		r := newRig(t)
		r.cluster.FailedJobs = true
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonMigration {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("readiness reached after several observations succeeds", func(t *testing.T) {
		r := newRig(t)
		r.cluster.ReadyAfterGets = 3
		if res := r.run(); !res.Completed {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("revoked authority before registration", func(t *testing.T) {
		r := newRig(t)
		r.fac.Revoke(r.attempt)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonAuthority || r.cluster.ApplyCalls != 0 {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("revocation at the apply intent stops effects", func(t *testing.T) {
		r := newRig(t)
		r.fac.RevokeAt("applying", r.attempt) // revoked after registration, before the intent is recorded
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonAuthority || r.cluster.ApplyCalls != 0 || r.cluster.Count() != 0 {
			t.Fatalf("%+v applies=%d", res, r.cluster.ApplyCalls)
		}
		if rec, _ := r.fac.Record("run_1:deploy"); rec.Status != "pending" {
			t.Fatalf("a revoked attempt changes nothing: %+v", rec)
		}
	})

	t.Run("revocation before the success report leaves the record unsettled", func(t *testing.T) {
		r := newRig(t)
		r.fac.RevokeAt("succeeded", r.attempt)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonAuthority {
			t.Fatalf("%+v", res)
		}
	})
}

func TestSnapshotDigestIsDeterministicAndCoversEverything(t *testing.T) {
	s := baseSnapshot("preview", ns)
	d1, _ := s.Digest()
	d2, _ := baseSnapshot("preview", ns).Digest()
	if d1 != d2 {
		t.Fatal("the digest must be deterministic")
	}
	mutations := map[string]func(*delivery.Snapshot){
		"config value":   func(s *delivery.Snapshot) { s.Config["REPLICAS"] = "2" },
		"namespace":      func(s *delivery.Snapshot) { s.Namespace = "other" },
		"credential v":   func(s *delivery.Snapshot) { s.Secrets[4].Version++ },
		"credential ref": func(s *delivery.Snapshot) { s.Secrets[4].Ref = "secret://project/other" },
		"ttl":            func(s *delivery.Snapshot) { s.TTLSeconds++ },
		"kind":           func(s *delivery.Snapshot) { s.Kind = "stage" },
	}
	for name, mut := range mutations {
		c := baseSnapshot("preview", ns)
		mut(&c)
		if d, _ := c.Digest(); d == d1 {
			t.Errorf("%s must change the identity", name)
		}
	}
	// Credential order does not matter: bindings are sorted by name.
	c := baseSnapshot("preview", ns)
	c.Secrets[0], c.Secrets[3] = c.Secrets[3], c.Secrets[0]
	if d, _ := c.Digest(); d != d1 {
		t.Error("binding order must not change the digest")
	}
}

// TestRuntimeConfigPromotion: the same image digest is promoted with distinct
// explicit configuration; retained inputs survive later edits; changed
// configuration or credentials change identity.
func TestRuntimeConfigPromotion(t *testing.T) {
	preview := newRig(t)
	if res := preview.run(); !res.Completed {
		t.Fatalf("%+v", res)
	}
	stageSnap := baseSnapshot("stage", "acme-stage")
	stageSnap.Environment = "env_stage"
	stageSnap.Config["REPLICAS"] = "3"
	stageSnap.Config["APP_URL"] = "http://inventory.stage.example.invalid"
	stageSnap.Secrets[4].Ref = "secret://project/erp-stage" // stage uses its own ERP credential
	stage := newRig(t, func(r *rig) { r.snapshot, r.attempt = stageSnap, "att_stage_1"; r.install() })
	if res := stage.run(); !res.Completed {
		t.Fatalf("%+v", res)
	}
	p, _ := preview.fac.Record("run_1:deploy")
	s, _ := stage.fac.Record("run_1:deploy")
	if p.Image.Digest != s.Image.Digest || p.Image.Digest != digest {
		t.Fatalf("the promoted image digest is the preview's: %s %s", p.Image.Digest, s.Image.Digest)
	}
	if p.ConfigurationDigest == s.ConfigurationDigest || p.Namespace == s.Namespace || p.EnvironmentID == s.EnvironmentID {
		t.Fatalf("stage carries its own explicit configuration identity:\n%+v\n%+v", p, s)
	}
	// Both rendered the very same image reference, different replica configuration.
	pl, sl := planFor(preview), planFor(stage)
	if pl.ImageRef() != sl.ImageRef() || pl.Replicas() == sl.Replicas() {
		t.Fatalf("%s/%d vs %s/%d", pl.ImageRef(), pl.Replicas(), sl.ImageRef(), sl.Replicas())
	}

	t.Run("a retained input survives a later edit; the edited copy is not the pinned one", func(t *testing.T) {
		r := newRig(t)
		pinned := r.in.SnapshotJSON
		edited := r.snapshot
		edited.Config = map[string]string{}
		for k, v := range r.snapshot.Config {
			edited.Config[k] = v
		}
		edited.Config["REPLICAS"] = "5" // edited after the run pinned its configuration
		raw, _ := json.Marshal(edited)
		r.in.SnapshotJSON = string(raw)
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonSnapshot || r.fac.Calls["register"] != 0 {
			t.Fatalf("an edited snapshot is not the pinned configuration: %+v", res)
		}
		r.in.SnapshotJSON = pinned
		if res := r.run(); !res.Completed {
			t.Fatalf("the retained pinned input still deploys: %+v", res)
		}
	})

	t.Run("changed credentials or configuration alter the environment identity", func(t *testing.T) {
		base := baseSnapshot("preview", ns)
		d0, _ := base.Digest()
		for name, mutate := range map[string]func(*delivery.Snapshot){
			"rotated credential version": func(s *delivery.Snapshot) { s.Secrets[3].Version++ },
			"replaced credential":        func(s *delivery.Snapshot) { s.Secrets[2].Ref = "secret://project/other" },
			"changed configuration":      func(s *delivery.Snapshot) { s.Config["ERP_BASE_URL"] = "https://erp2.example.invalid" },
		} {
			c := baseSnapshot("preview", ns)
			mutate(&c)
			if d, _ := c.Digest(); d == d0 {
				t.Errorf("%s must change the identity", name)
			}
		}
	})
}

func planFor(r *rig) delivery.Plan {
	return delivery.Plan{Namespace: r.snapshot.Namespace, ImageRepo: repo, ImageDigest: digest, Config: r.snapshot.Config}
}

// TestSnapshotBindingContract: every snapshot binding must exist, identically,
// in the pinned context (the factory's CheckEnvironment rule); the context may
// declare further bindings the script never consumes.
func TestSnapshotBindingContract(t *testing.T) {
	t.Run("a snapshot binding absent from the context fails before effects", func(t *testing.T) {
		r := newRig(t)
		secrets := r.cx["secrets"].([]map[string]any)
		r.cx["secrets"] = append(append([]map[string]any{}, secrets[:3]...), secrets[4:]...) // CURSOR_KEY removed
		r.fac.SetContext(r.attempt, r.cx)
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonSnapshot {
			t.Fatalf("want %s, got %+v", delivery.ReasonSnapshot, res)
		}
		if r.fac.Calls["register"] != 0 || r.cluster.ApplyCalls != 0 || r.cluster.Count() != 0 {
			t.Fatalf("nothing may be registered or applied:\n%s", r.events())
		}
	})
	t.Run("a binding differing in each field fails before effects", func(t *testing.T) {
		for field, v := range map[string]any{"ref": "secret://project/other", "version": int64(9), "name": "OTHER", "delivery": "env"} {
			r := newRig(t)
			secrets := r.cx["secrets"].([]map[string]any)
			secrets[3][field] = v
			r.fac.SetContext(r.attempt, r.cx)
			if res := r.run(); res.Completed || res.Reason != delivery.ReasonSnapshot || r.fac.Calls["register"] != 0 || r.cluster.ApplyCalls != 0 {
				t.Errorf("%s: %+v", field, res)
			}
		}
	})
	t.Run("duplicate or invalid snapshot binding names fail before effects", func(t *testing.T) {
		for name, mutate := range map[string]func(*rig){
			"duplicate": func(r *rig) {
				r.snapshot.Secrets = append(r.snapshot.Secrets, r.snapshot.Secrets[4])
			},
			"invalid": func(r *rig) {
				r.snapshot.Secrets[4].Name = "../ERP_TOKEN"
			},
		} {
			r := newRig(t, mutate, func(r *rig) { r.install() })
			if res := r.run(); res.Completed || res.Reason != delivery.ReasonSnapshot || r.fac.Calls["register"] != 0 || r.cluster.ApplyCalls != 0 {
				t.Errorf("%s: %+v\n%s", name, res, r.events())
			}
		}
	})
	t.Run("a missing deployment credential fails before effects", func(t *testing.T) {
		r := newRig(t, func(r *rig) {
			r.snapshot.Kubeconfig = "NOT_BOUND"
			r.install()
		})
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonSnapshot || r.fac.Calls["register"] != 0 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("an unrelated context binding is permitted and never consumed", func(t *testing.T) {
		r := newRig(t)
		r.cx["secrets"] = append(r.cx["secrets"].([]map[string]any),
			map[string]any{"ref": "secret://project/unrelated", "version": int64(1), "name": "UNRELATED", "delivery": "file"})
		r.fac.SetContext(r.attempt, r.cx)
		_ = os.WriteFile(filepath.Join(r.secrets, "UNRELATED"), []byte("must-not-be-read\n"), 0o600)
		res := r.run()
		if !res.Completed {
			t.Fatalf("%+v\n%s", res, r.events())
		}
		raw, _ := r.cluster.LastApplied()
		if strings.Contains(string(raw), "must-not-be-read") {
			t.Fatal("an unrelated binding was consumed")
		}
	})
}

// TestDeployOwnershipAnnotations: deployment, action and configuration
// annotations must all match on first execution and on settled replay.
func TestDeployOwnershipAnnotations(t *testing.T) {
	refs := []delivery.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "inventory-api"}}
	t.Run("a receipt that omits or contradicts the action or configuration annotation is refused", func(t *testing.T) {
		for _, drop := range []string{delivery.AnnotationActionKey, delivery.AnnotationConfigDigest} {
			r := newRig(t)
			id := factoryfake.DeploymentID(tenant, project, "run_1:deploy")
			d, _ := r.snapshot.Digest()
			ann := map[string]string{delivery.AnnotationDeploymentID: id, delivery.AnnotationActionKey: "run_1:deploy", delivery.AnnotationConfigDigest: d}
			ann[drop] = "someone-else"
			r.fac.Plant("run_1:deploy", delivery.Record{ID: id, EnvironmentID: envID, ConfigurationDigest: d, Namespace: ns, ActionKey: "run_1:deploy", Status: "pending", Reason: "registered",
				Source: delivery.Source{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: commit}, Image: delivery.Image{ArtifactID: "art_image_1", Digest: digest}}, ann)
			if res := r.run(); res.Completed || res.Reason != delivery.ReasonReceipt || r.cluster.ApplyCalls != 0 {
				t.Errorf("%s: %+v", drop, res)
			}
		}
	})
	t.Run("first execution refuses a ready object with the right deployment id but a foreign action or configuration", func(t *testing.T) {
		for _, key := range []string{delivery.AnnotationActionKey, delivery.AnnotationConfigDigest} {
			r := newRig(t)
			id := factoryfake.DeploymentID(tenant, project, "run_1:deploy")
			d, _ := r.snapshot.Digest()
			ann := map[string]string{delivery.AnnotationDeploymentID: id, delivery.AnnotationActionKey: "run_1:deploy", delivery.AnnotationConfigDigest: d}
			ann[key] = "other"
			r.cluster.Preexist(refs[0], ann)
			res := r.run()
			if res.Completed || res.Reason != delivery.ReasonForeign || r.cluster.ApplyCalls != 0 {
				t.Errorf("%s: %+v applies=%d", key, res, r.cluster.ApplyCalls)
			}
		}
	})
	t.Run("settled replay never reports success over a foreign live object", func(t *testing.T) {
		r := newRig(t)
		if res := r.run(); !res.Completed {
			t.Fatalf("%+v", res)
		}
		// Someone replaces an object with a ready one that names another action.
		ann, _, _ := r.cluster.Object(refs[0])
		replaced := map[string]string{}
		for k, v := range ann {
			replaced[k] = v
		}
		replaced[delivery.AnnotationActionKey] = "run_9:deploy"
		r.cluster.Preexist(refs[0], replaced)
		before := r.cluster.ApplyCalls
		res := r.run()
		if res.Completed || res.Reason != delivery.ReasonForeign || r.cluster.ApplyCalls != before {
			t.Fatalf("%+v applies %d -> %d", res, before, r.cluster.ApplyCalls)
		}
		if res.Evidence != nil {
			t.Fatal("no success evidence may accompany a foreign object")
		}
	})
	t.Run("settled replay with a different configuration annotation is not live", func(t *testing.T) {
		r := newRig(t)
		if res := r.run(); !res.Completed {
			t.Fatalf("%+v", res)
		}
		ann, _, _ := r.cluster.Object(refs[0])
		replaced := map[string]string{}
		for k, v := range ann {
			replaced[k] = v
		}
		replaced[delivery.AnnotationConfigDigest] = "sha256:" + strings.Repeat("f", 64)
		r.cluster.Preexist(refs[0], replaced)
		if res := r.run(); res.Completed || res.Reason != delivery.ReasonForeign {
			t.Fatalf("%+v", res)
		}
	})
}
