package delivery_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	scope = domain.Scope{TenantID: "t035_acme", ProjectID: "p035_inventory"}
	now   = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
)

func kube(ref string) delivery.Credential {
	return delivery.Credential{Name: "KUBECONFIG", Ref: ref, Delivery: runtimev1.DeliveryFile}
}

func record() delivery.EnvironmentRecord {
	return delivery.EnvironmentRecord{
		Scope: scope, ID: "env_1", Name: "preview", Kind: delivery.KindPreview, Target: "kind", Namespace: "preview",
		Kubeconfig: kube("secret://project/kubeconfig"), Secrets: []delivery.Credential{{Name: "TOKEN", Ref: "secret://project/token", Delivery: runtimev1.DeliveryEnv}},
		Config: map[string]string{"REPLICAS": "1"}, PreviewTTL: 24 * time.Hour,
	}
}

func newEnv(t *testing.T) delivery.Environment {
	t.Helper()
	env, err := delivery.NewEnvironment(record(), now)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestEnvironmentValidation(t *testing.T) {
	bad := map[string]func(*delivery.EnvironmentRecord){
		"name":              func(r *delivery.EnvironmentRecord) { r.Name = "Preview" },
		"kind":              func(r *delivery.EnvironmentRecord) { r.Kind = "prod" },
		"preview needs ttl": func(r *delivery.EnvironmentRecord) { r.PreviewTTL = 0 },
		"ttl too long":      func(r *delivery.EnvironmentRecord) { r.PreviewTTL = 365 * 24 * time.Hour },
		"stage has no ttl":  func(r *delivery.EnvironmentRecord) { r.Kind = delivery.KindStage },
		"fractional ttl":    func(r *delivery.EnvironmentRecord) { r.PreviewTTL = time.Hour + time.Millisecond },
		"target":            func(r *delivery.EnvironmentRecord) { r.Target = "-x" },
		"namespace":         func(r *delivery.EnvironmentRecord) { r.Namespace = "Not_A_Label" },
		"kubeconfig ref":    func(r *delivery.EnvironmentRecord) { r.Kubeconfig.Ref = "secret://tenant/kubeconfig" },
		"kubeconfig form":   func(r *delivery.EnvironmentRecord) { r.Kubeconfig.Delivery = "inline" },
		"duplicate name":    func(r *delivery.EnvironmentRecord) { r.Secrets[0].Name = "KUBECONFIG" },
		"aliased ref":       func(r *delivery.EnvironmentRecord) { r.Secrets[0].Ref = r.Kubeconfig.Ref },
		"config key":        func(r *delivery.EnvironmentRecord) { r.Config = map[string]string{"1bad": "x"} },
		"config value":      func(r *delivery.EnvironmentRecord) { r.Config = map[string]string{"K": strings.Repeat("x", 2000)} },
		"too many config": func(r *delivery.EnvironmentRecord) {
			r.Config = map[string]string{}
			for i := range 65 {
				r.Config["K"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
			}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := record()
			r.Secrets = append([]delivery.Credential(nil), r.Secrets...)
			mutate(&r)
			if _, err := delivery.NewEnvironment(r, now); !errors.Is(err, delivery.ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	stage := record()
	stage.Kind, stage.PreviewTTL = delivery.KindStage, 0
	if _, err := delivery.NewEnvironment(stage, now); err != nil {
		t.Fatalf("a stage environment without a TTL: %v", err)
	}
}

func TestEnvironmentEdit(t *testing.T) {
	env := newEnv(t)
	ns := "other"
	changed, err := env.Edit(1, delivery.EnvironmentEdit{Namespace: &ns}, now.Add(time.Minute))
	if err != nil || !changed || env.Version() != 2 || env.Record().Namespace != "other" || !env.Record().UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("edit: %v changed=%v %+v", err, changed, env.Record())
	}
	if _, err := env.Edit(1, delivery.EnvironmentEdit{Namespace: &ns}, now); !errors.Is(err, delivery.ErrVersionConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	// An edit that changes nothing leaves the version alone.
	if changed, err := env.Edit(2, delivery.EnvironmentEdit{Namespace: &ns}, now); err != nil || changed || env.Version() != 2 {
		t.Fatalf("no-op edit: %v %v v%d", err, changed, env.Version())
	}
	// An invalid edit changes nothing.
	bad := "NOT VALID"
	if _, err := env.Edit(2, delivery.EnvironmentEdit{Namespace: &bad}, now); !errors.Is(err, delivery.ErrInvalid) || env.Record().Namespace != "other" {
		t.Fatalf("invalid edit: %v", err)
	}
	// The identity, name and kind are not editable: there is no field for them.
	if rec := env.Record(); rec.Name != "preview" || rec.Kind != delivery.KindPreview || rec.ID != "env_1" {
		t.Fatalf("identity changed: %+v", rec)
	}
	// Returned records are copies.
	rec := env.Record()
	rec.Config["REPLICAS"] = "99"
	rec.Secrets[0].Name = "HIJACKED"
	if env.Record().Config["REPLICAS"] != "1" || env.Record().Secrets[0].Name != "TOKEN" {
		t.Fatal("Record exposed internal state")
	}
}

func TestSnapshotIdentity(t *testing.T) {
	env := newEnv(t)
	versions := map[string]int64{"secret://project/kubeconfig": 3, "secret://project/token": 1}
	a, err := delivery.NewSnapshot(env, versions, now)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic: the same effective configuration has one identity, whatever
	// the time or the map ordering of the inputs.
	b, err := delivery.NewSnapshot(env, versions, now.Add(72*time.Hour))
	if err != nil || a.Digest != b.Digest {
		t.Fatalf("not deterministic: %v %s %s", err, a.Digest, b.Digest)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Pin() != (runtimev1.EnvironmentPin{ID: "env_1", ConfigDigest: a.Digest}) || len(a.Secrets) != 2 || a.Secrets[0].Name != "KUBECONFIG" || a.Secrets[0].Version != 3 {
		t.Fatalf("pin or bindings: %+v", a)
	}
	if kc, ok := a.KubeconfigBinding(); !ok || kc.Ref != "secret://project/kubeconfig" {
		t.Fatalf("kubeconfig binding: %+v", kc)
	}

	// Every covered member changes the identity.
	variants := map[string]func() (delivery.ConfigurationSnapshot, error){
		"version": func() (delivery.ConfigurationSnapshot, error) {
			return delivery.NewSnapshot(env, map[string]int64{"secret://project/kubeconfig": 4, "secret://project/token": 1}, now)
		},
		"namespace": func() (delivery.ConfigurationSnapshot, error) {
			e := env
			ns := "other"
			if _, err := e.Edit(1, delivery.EnvironmentEdit{Namespace: &ns}, now); err != nil {
				return delivery.ConfigurationSnapshot{}, err
			}
			return delivery.NewSnapshot(e, versions, now)
		},
		"config": func() (delivery.ConfigurationSnapshot, error) {
			e := newEnv(t)
			c := map[string]string{"REPLICAS": "2"}
			if _, err := e.Edit(1, delivery.EnvironmentEdit{Config: &c}, now); err != nil {
				return delivery.ConfigurationSnapshot{}, err
			}
			return delivery.NewSnapshot(e, versions, now)
		},
		"ttl": func() (delivery.ConfigurationSnapshot, error) {
			e := newEnv(t)
			d := 2 * time.Hour
			if _, err := e.Edit(1, delivery.EnvironmentEdit{PreviewTTL: &d}, now); err != nil {
				return delivery.ConfigurationSnapshot{}, err
			}
			return delivery.NewSnapshot(e, versions, now)
		},
		"credential form": func() (delivery.ConfigurationSnapshot, error) {
			e := newEnv(t)
			k := kube("secret://project/kubeconfig")
			k.Delivery = runtimev1.DeliveryEnv
			if _, err := e.Edit(1, delivery.EnvironmentEdit{Kubeconfig: &k}, now); err != nil {
				return delivery.ConfigurationSnapshot{}, err
			}
			return delivery.NewSnapshot(e, versions, now)
		},
		"scope": func() (delivery.ConfigurationSnapshot, error) {
			r := record()
			r.Scope.ProjectID = "p035_other"
			e, err := delivery.NewEnvironment(r, now)
			if err != nil {
				return delivery.ConfigurationSnapshot{}, err
			}
			return delivery.NewSnapshot(e, versions, now)
		},
	}
	for name, mk := range variants {
		s, err := mk()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Digest == a.Digest {
			t.Errorf("a change of %s did not change the snapshot identity", name)
		}
	}

	// A credential without a pinned version cannot be snapshotted.
	if _, err := delivery.NewSnapshot(env, map[string]int64{"secret://project/kubeconfig": 3}, now); !errors.Is(err, delivery.ErrCredentialUnavailable) {
		t.Fatalf("a missing version: %v", err)
	}
	// A tampered snapshot fails validation: the digest covers its content.
	forged := a
	forged.Namespace = "kube-system"
	if err := forged.Validate(); !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("a tampered snapshot validated: %v", err)
	}
	forged = a
	forged.Secrets = append([]runtimev1.SecretBinding(nil), a.Secrets...)
	forged.Secrets[0].Version = 9
	if err := forged.Validate(); !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("tampered bindings validated: %v", err)
	}
	// The digest never covers a secret value or a timestamp: its inputs are
	// references only.
	if strings.Contains(string(a.Digest), "secret") {
		t.Fatal("digest leaks references")
	}
}

func domain2(tenant string) domain.Scope {
	return domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: scope.ProjectID}
}
