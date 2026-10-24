package artifactshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

type fakePublic struct {
	mu    sync.Mutex
	calls []publicCall
	view  func(publicCall) (artifacts.View, error)
}

type publicCall struct {
	caller   artifacts.Caller
	scope    domain.Scope
	id       domain.ArtifactID
	download bool
}

func (f *fakePublic) Metadata(_ context.Context, c artifacts.Caller, s domain.Scope, id domain.ArtifactID, dl bool) (artifacts.View, error) {
	f.mu.Lock()
	pc := publicCall{c, s, id, dl}
	f.calls = append(f.calls, pc)
	h := f.view
	f.mu.Unlock()
	return h(pc)
}

func (f *fakePublic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func publicServer(t *testing.T, svc artifactshttp.PublicService, authz httpapi.Authorizer) *httptest.Server {
	t.Helper()
	if authz == nil {
		authz = allowAll{}
	}
	r := router(t, "/api/v1", authz)
	h, err := artifactshttp.NewPublic(svc, artifactshttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(r); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

var (
	pubScope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	pubProv  = artifacts.Provenance{RepositoryURL: "https://git.example.test/acme/app.git", Commit: strings.Repeat("a", 40)}
	pubT0    = time.Date(2026, 5, 6, 7, 8, 9, 123456000, time.UTC)
)

const stagingSecret = "artifacts/t_acme/p_inventory/art_pub/staging/STAGINGNONCE"
const sealedSecret = "artifacts/t_acme/p_inventory/art_pub/sealed/SEALEDNONCE"

func usableView(t *testing.T, download bool) artifacts.View {
	t.Helper()
	data := []byte("public bytes")
	size := int64(len(data))
	res := artifacts.Reservation{Scope: pubScope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "report.txt", Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
		Digest: artifacts.DigestOf(data), MediaType: "text/plain", Size: &size, Provenance: pubProv}
	a, err := artifacts.NewFile("art_pub", res, stagingSecret, pubT0.Add(time.Hour), pubT0)
	if err != nil {
		t.Fatal(err)
	}
	a, err = a.Complete(artifacts.Verification{Digest: artifacts.DigestOf(data), Size: size}, sealedSecret, pubT0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	v := artifacts.View{Artifact: a}
	if download {
		v.Download = &artifacts.Transfer{Method: "GET", URL: "https://s3.example.invalid/bucket/obj?X-Amz-Signature=abc", ExpiresAt: pubT0.Add(10 * time.Minute)}
	}
	return v
}

func ociView(t *testing.T) artifacts.View {
	t.Helper()
	d := artifacts.DigestOf([]byte("manifest"))
	res := artifacts.Reservation{Scope: pubScope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "image", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput,
		Digest: d, Registry: "registry.example.test/team/app", Provenance: pubProv}
	a, err := artifacts.NewRegistered("art_oci", res, artifacts.OCIVerification{Repository: "registry.example.test/team/app", Digest: d, ManifestSize: 321, Provenance: pubProv}, pubT0)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts.View{Artifact: a}
}

func reservedView(t *testing.T) artifacts.View {
	t.Helper()
	size := int64(3)
	res := artifacts.Reservation{Scope: pubScope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "log", Kind: artifacts.KindFile, Role: artifacts.RoleDiagnostic,
		Digest: artifacts.DigestOf([]byte("abc")), MediaType: "text/plain", Size: &size}
	a, err := artifacts.NewFile("art_res", res, stagingSecret, pubT0.Add(time.Hour), pubT0)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts.View{Artifact: a}
}

type docMap = map[string]any

func loadYAML(t *testing.T, path string) docMap {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d docMap
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func at(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// jsonFields returns the JSON names of a struct's fields and which of them are
// written without omitempty.
func jsonFields(t reflect.Type) (all, always []string) {
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		all = append(all, name)
		if !strings.Contains(opts, "omitempty") {
			always = append(always, name)
		}
	}
	sort.Strings(all)
	sort.Strings(always)
	return
}

// TestPublicMetadataContract: OpenAPI and generated DTO alignment, common
// envelopes, no raw credentials or storage internals, and authorization before
// download issuance. No external services.
func TestPublicMetadataContract(t *testing.T) {
	spec := loadYAML(t, "../../../api/artifacts.yaml")
	schemas := at(spec, "components", "schemas")

	t.Run("the fragment defines the representation without adding a route", func(t *testing.T) {
		paths := at(spec, "paths").(map[string]any)
		if len(paths) != 1 {
			t.Fatalf("the fragment must add no other route: %v", paths)
		}
		const p = "/tenants/{tenantId}/projects/{projectId}/artifacts/{id}"
		if at(spec, "paths", p, "get", "operationId") != "getArtifact" || at(spec, "paths", p, "post") != nil || at(spec, "paths", p, "put") != nil || at(spec, "paths", p, "delete") != nil {
			t.Fatalf("operations: %v", paths[p])
		}
		for _, code := range []string{"400", "401", "403", "404", "409", "429", "500", "503"} {
			if at(spec, "paths", p, "get", "responses", code) == nil {
				t.Errorf("missing %s response", code)
			}
		}
		if sec := at(spec, "paths", p, "get", "security"); sec == nil {
			t.Error("the operation must declare its security requirement")
		}
		// Shared envelopes come from the common fragment.
		for _, code := range []string{"400", "401", "403", "404", "429", "500", "503"} {
			ref, _ := at(spec, "paths", p, "get", "responses", code, "$ref").(string)
			if !strings.HasPrefix(ref, "common.yaml#/components/responses/") {
				t.Errorf("%s does not reuse the common envelope: %q", code, ref)
			}
		}
		if ref, _ := at(spec, "paths", p, "get", "responses", "409", "content", "application/json", "schema", "$ref").(string); ref != "common.yaml#/components/schemas/ErrorEnvelope" {
			t.Errorf("409 envelope: %q", ref)
		}
		if at(spec, "paths", p, "get", "responses", "200", "headers", "ETag") == nil {
			t.Error("200 must document the ETag")
		}
	})

	t.Run("the generated DTO matches the schema field for field", func(t *testing.T) {
		props := at(schemas, "Artifact", "properties").(map[string]any)
		var specNames []string
		for k := range props {
			specNames = append(specNames, k)
		}
		sort.Strings(specNames)
		all, always := jsonFields(reflect.TypeOf(artifactshttp.Artifact{}))
		if !reflect.DeepEqual(specNames, all) {
			t.Fatalf("schema properties %v, generated fields %v", specNames, all)
		}
		required := strs(at(schemas, "Artifact", "required"))
		sort.Strings(required)
		if !reflect.DeepEqual(required, always) || !reflect.DeepEqual(required, specNames) {
			t.Fatalf("required %v, always-written fields %v", required, always)
		}
		if at(schemas, "Artifact", "additionalProperties") != false {
			t.Error("the representation is closed")
		}
		for name, typ := range map[string]reflect.Type{
			"ArtifactDownload": reflect.TypeOf(artifactshttp.ArtifactDownload{}), "SourceProvenance": reflect.TypeOf(artifactshttp.SourceProvenance{}),
		} {
			pr := at(schemas, name, "properties").(map[string]any)
			var names []string
			for k := range pr {
				names = append(names, k)
			}
			sort.Strings(names)
			all, _ := jsonFields(typ)
			if !reflect.DeepEqual(names, all) {
				t.Errorf("%s: schema %v, generated %v", name, names, all)
			}
		}
	})

	t.Run("enums and patterns equal the module and the runtime contract", func(t *testing.T) {
		eq := func(name string, got []string, want ...string) {
			t.Helper()
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: schema %v, code %v", name, got, want)
			}
		}
		eq("kind", strs(at(schemas, "ArtifactKind", "enum")), string(artifacts.KindFile), string(artifacts.KindOCI))
		if string(artifacts.KindFile) != string(runtimev1.KindFile) || string(artifacts.KindOCI) != string(runtimev1.KindOCI) || string(artifacts.RoleBundle) != string(runtimev1.RoleBundle) ||
			string(artifacts.StateUsable) != string(runtimev1.StateUsable) || string(artifacts.StateRegistered) != string(runtimev1.StateRegistered) || string(artifacts.StateReserved) != string(runtimev1.StateReserved) {
			t.Error("module enumerations drifted from the frozen runtime enumerations")
		}
		eq("role", strs(at(schemas, "ArtifactRole", "enum")), string(artifacts.RoleOutput), string(artifacts.RoleDiagnostic), string(artifacts.RoleBundle))
		eq("state", strs(at(schemas, "ArtifactState", "enum")), string(artifacts.StateReserved), string(artifacts.StateRegistered), string(artifacts.StateUsable))
		eq("availability", strs(at(schemas, "ArtifactAvailability", "enum")), string(artifactshttp.Available), string(artifactshttp.Abandoned), string(artifactshttp.Removed))
		if at(schemas, "Digest", "pattern") != runtimev1.Patterns["Digest"] {
			t.Error("the digest pattern differs from the runtime contract")
		}
		if at(schemas, "Artifact", "properties", "name", "pattern") != runtimev1.Patterns["SafeName"] {
			t.Error("the name pattern differs from the runtime contract")
		}
		if at(schemas, "SourceProvenance", "properties", "commit", "pattern") != runtimev1.Patterns["GitCommit"] ||
			at(schemas, "SourceProvenance", "properties", "repositoryUrl", "pattern") != runtimev1.Patterns["RepositoryURL"] {
			t.Error("the provenance patterns differ from the runtime contract")
		}
		if at(schemas, "ArtifactDownload", "properties", "url", "pattern") != runtimev1.Patterns["UploadURL"] {
			t.Error("the transfer URL pattern differs from the runtime contract")
		}
		if got := at(schemas, "Artifact", "properties", "sizeBytes", "maximum"); got != 1<<40 {
			t.Errorf("size bound: %v", got)
		}
		// The representation names no storage internals.
		forbidden := []string{"key", "bucket", "stagingKey", "sealedKey", "storageKey", "objectKey", "claimToken", "etag", "credentials", "token", "secret", "upload"}
		for name := range at(schemas, "Artifact", "properties").(map[string]any) {
			for _, f := range forbidden {
				if strings.EqualFold(name, f) {
					t.Errorf("property %q exposes a storage internal", name)
				}
			}
		}
	})

	t.Run("responses are complete, safe and carry the version", func(t *testing.T) {
		views := map[string]artifacts.View{"usable file with download": usableView(t, true), "usable file": usableView(t, false), "registration": ociView(t), "reserved": reservedView(t)}
		for name, v := range views {
			svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return v, nil }}
			srv := publicServer(t, svc, nil)
			r := call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/"+string(v.Artifact.ID()), "user:alice", nil)
			if r.status != 200 {
				t.Fatalf("%s: %d %s", name, r.status, r.raw)
			}
			var keys map[string]json.RawMessage
			r.json(t, &keys)
			_, always := jsonFields(reflect.TypeOf(artifactshttp.Artifact{}))
			for _, k := range always {
				if _, ok := keys[k]; !ok {
					t.Errorf("%s: %q is missing (nullable fields are explicit null)", name, k)
				}
			}
			dec := json.NewDecoder(bytes.NewReader(r.raw))
			dec.DisallowUnknownFields()
			var dto artifactshttp.Artifact
			if err := dec.Decode(&dto); err != nil {
				t.Fatalf("%s: the response is not the generated representation: %v", name, err)
			}
			for _, internal := range []string{stagingSecret, sealedSecret, "STAGINGNONCE", "SEALEDNONCE", "artifacts/t_acme", "staging", "sealed", "claim", "bucket/obj?"} {
				if internal == "bucket/obj?" && v.Download != nil {
					continue // the download URL is the one deliberate instruction
				}
				if strings.Contains(string(r.raw), internal) {
					t.Errorf("%s: the body discloses %q: %s", name, internal, r.raw)
				}
			}
			if etag := r.header.Get("ETag"); etag != `"`+itoa(int64(v.Artifact.Version()))+`"` {
				t.Errorf("%s: ETag %q for version %d", name, etag, v.Artifact.Version())
			}
			if r.header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(r.header.Get("X-Request-ID"), "req_") {
				t.Errorf("%s: headers %v", name, r.header)
			}
			if (dto.Download != nil) != (v.Download != nil) {
				t.Errorf("%s: download presence", name)
			}
			if string(dto.Digest) != string(v.Artifact.Digest()) || string(dto.State) != string(v.Artifact.State()) || dto.Availability != artifactshttp.Available {
				t.Errorf("%s: %+v", name, dto)
			}
			switch v.Artifact.Kind() {
			case artifacts.KindOCI:
				if dto.Registry == nil || *dto.Registry != "registry.example.test/team/app" || dto.MediaType != nil || dto.SizeBytes == nil || *dto.SizeBytes != 321 || dto.Download != nil {
					t.Errorf("%s: %+v", name, dto)
				}
			default:
				if dto.Registry != nil || dto.MediaType == nil || *dto.MediaType != "text/plain" || dto.SizeBytes == nil {
					t.Errorf("%s: %+v", name, dto)
				}
			}
			if v.Artifact.State() == artifacts.StateReserved && (dto.VerifiedAt != nil || dto.Source != nil) {
				t.Errorf("%s: an unverified reservation claims verification or provenance: %+v", name, dto)
			}
			if v.Artifact.State() == artifacts.StateUsable && (dto.VerifiedAt == nil || dto.Source == nil || dto.Source.Commit != pubProv.Commit) {
				t.Errorf("%s: %+v", name, dto)
			}
			if dto.Download != nil && (dto.Download.Method != artifactshttp.GET || !dto.Download.ExpiresAt.Equal(v.Download.ExpiresAt)) {
				t.Errorf("%s: download %+v", name, dto.Download)
			}
		}
	})

	t.Run("authorization precedes any module call and download issuance", func(t *testing.T) {
		svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return usableView(t, true), nil }}
		for name, tc := range map[string]struct {
			deny   error
			status int
			code   string
		}{
			"denied": {httpapi.ErrDenied, 403, "forbidden"},
			"hidden": {httpapi.ErrHidden, 404, "not_found"},
			"outage": {errors.New("policy engine down"), 503, "unavailable"},
		} {
			srv := publicServer(t, svc, &authzRecorder{deny: tc.deny})
			before := svc.count()
			r := call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub?download=true", "user:alice", nil)
			if r.status != tc.status || r.errorCode(t) != tc.code {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
			if svc.count() != before {
				t.Errorf("%s: the module (and so a download instruction) was reached before authorization", name)
			}
		}
		az := &authzRecorder{}
		srv := publicServer(t, svc, az)
		call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub", "user:alice", nil)
		want := httpapi.Target{TenantID: "t_acme", ProjectID: "p_inventory", Resource: httpapi.Reference{Kind: "artifact", ID: "art_pub"}}
		if len(az.seen) != 1 || az.seen[0] != want || az.acts[0] != "artifact.read" {
			t.Fatalf("authorization target: %+v %v", az.seen, az.acts)
		}
		if r := call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub", "", nil); r.status != 401 {
			t.Errorf("anonymous: %d", r.status)
		}
	})

	t.Run("the module sees the verified principal and the exact scope", func(t *testing.T) {
		svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return usableView(t, false), nil }}
		srv := publicServer(t, svc, nil)
		call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub", "user:alice", nil)
		call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub?download=true", "user:alice", nil)
		call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub?download=false", "user:bob", nil)
		want := []publicCall{
			{artifacts.Caller{Subject: "user:alice"}, pubScope, "art_pub", false},
			{artifacts.Caller{Subject: "user:alice"}, pubScope, "art_pub", true},
			{artifacts.Caller{Subject: "user:bob"}, pubScope, "art_pub", false},
		}
		if !reflect.DeepEqual(svc.calls, want) {
			t.Fatalf("calls: %+v", svc.calls)
		}
	})

	t.Run("query and path validation", func(t *testing.T) {
		svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return usableView(t, false), nil }}
		srv := publicServer(t, svc, nil)
		base := srv.URL + "/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub"
		for _, q := range []string{"?download=maybe", "?download=1", "?download=TRUE", "?download=true&download=true", "?download=", "?download=true&limit=5", "?cursor=x", "?expand=all"} {
			if r := call(t, "GET", base+q, "user:alice", nil); r.status != 400 || r.errorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", q, r.status, r.raw)
			}
		}
		for _, p := range []string{"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/bad%20id", "/api/v1/tenants/-bad/projects/p_inventory/artifacts/art_pub", "/api/v1/tenants/t_acme/projects/%2e%2e/artifacts/art_pub"} {
			if r := call(t, "GET", srv.URL+p, "user:alice", nil); r.status != 400 && r.status != 404 {
				t.Errorf("%s: %d", p, r.status)
			}
		}
		if svc.count() != 0 {
			t.Fatalf("an invalid request reached the module %d times", svc.count())
		}
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			if r := call(t, m, base, "user:alice", nil); r.status != 405 && r.status != 404 {
				t.Errorf("%s: %d", m, r.status)
			}
		}
		if _, err := artifactshttp.NewPublic(nil, artifactshttp.DefaultActions()); err == nil {
			t.Error("a public handler without a service")
		}
	})

	t.Run("module errors map to the common envelope", func(t *testing.T) {
		for _, tc := range []struct {
			err    error
			status int
			code   string
		}{
			{artifacts.ErrNotFound, 404, "not_found"},
			{artifacts.ErrForbidden, 403, "forbidden"},
			{artifacts.ErrIncomplete, 409, "artifact_incomplete"},
			{artifacts.ErrUnavailable, 503, "unavailable"},
			{errors.New("dial tcp 10.0.0.1:5432: connection refused"), 500, "internal_error"},
		} {
			err := tc.err
			svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return artifacts.View{}, err }}
			srv := publicServer(t, svc, nil)
			r := call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/art_pub?download=true", "user:alice", nil)
			if r.status != tc.status || r.errorCode(t) != tc.code {
				t.Errorf("%v: %d %s", tc.err, r.status, r.raw)
			}
			if strings.Contains(string(r.raw), "10.0.0.1") {
				t.Errorf("the body discloses a cause: %s", r.raw)
			}
			var env httpapi.ErrorEnvelope
			r.json(t, &env)
			if env.Error.Details == nil || !strings.HasPrefix(env.Error.RequestId, "req_") {
				t.Errorf("incomplete envelope: %s", r.raw)
			}
		}
	})

	t.Run("runtime reference conversion preserves ownership and usability", func(t *testing.T) {
		u, o, r := usableView(t, false).Artifact, ociView(t).Artifact, reservedView(t).Artifact
		refs := artifactshttp.RuntimeRefs(map[domain.ArtifactID]artifacts.Reference{u.ID(): u.Reference(), o.ID(): o.Reference(), r.ID(): r.Reference()})
		if x := refs[u.ID()]; !x.Usable || x.Kind != runtimev1.KindFile || x.Scope != pubScope || x.Attempt != "att_1" {
			t.Errorf("usable: %+v", x)
		}
		if x := refs[o.ID()]; !x.Usable || x.Kind != runtimev1.KindOCI {
			t.Errorf("registration: %+v", x)
		}
		if x := refs[r.ID()]; x.Usable {
			t.Errorf("reserved: %+v", x)
		}
	})
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
