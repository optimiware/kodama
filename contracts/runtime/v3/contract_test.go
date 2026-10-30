package runtimev3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	schemaURLv1  = "https://kodama.invalid/contracts/runtime/v1/schema.json"
	schemaURLv2  = "https://kodama.invalid/contracts/runtime/v2/schema.json"
	schemaURLv3  = "https://kodama.invalid/contracts/runtime/v3/schema.json"
	schemaURLGit = "https://kodama.invalid/contracts/git/v1/schema.json"
)

// decoders maps every message type to the strict (request) or tolerant
// (response) decoder consumers use, so each fixture runs through the same code.
var decoders = map[string]func([]byte) error{
	"AdmitRequest":                func(b []byte) error { _, err := DecodeRequest[AdmitRequest](b); return err },
	"TokenRequest":                func(b []byte) error { _, err := DecodeRequest[TokenRequest](b); return err },
	"SecretResolveRequest":        func(b []byte) error { _, err := DecodeRequest[SecretResolveRequest](b); return err },
	"ArtifactReserveRequest":      func(b []byte) error { _, err := DecodeRequest[ArtifactReserveRequest](b); return err },
	"ArtifactCompleteRequest":     func(b []byte) error { _, err := DecodeRequest[ArtifactCompleteRequest](b); return err },
	"ResourceTokenRequest":        func(b []byte) error { _, err := DecodeRequest[ResourceTokenRequest](b); return err },
	"ResourceAdmitRequest":        func(b []byte) error { _, err := DecodeRequest[ResourceAdmitRequest](b); return err },
	"PreparationReceiptRequestV3": func(b []byte) error { _, err := DecodeRequest[PreparationReceiptRequestV3](b); return err },
	"BeginRequestV3":              func(b []byte) error { _, err := DecodeRequest[BeginRequestV3](b); return err },
	"GitSeedRequest":              func(b []byte) error { _, err := DecodeRequest[GitSeedRequest](b); return err },
	"GitIntegrationRequest":       func(b []byte) error { _, err := DecodeRequest[GitIntegrationRequest](b); return err },
	"ResultRequestV3":             func(b []byte) error { _, err := DecodeRequest[ResultRequestV3](b); return err },
	// ResultV3 is a nested message of the request, decoded strictly like the
	// request that carries it.
	"ResultV3": func(b []byte) error { _, err := DecodeRequest[ResultV3](b); return err },

	"TokenResponse":              func(b []byte) error { _, err := DecodeResponse[TokenResponse](b); return err },
	"SecretResolveResponse":      func(b []byte) error { _, err := DecodeResponse[SecretResolveResponse](b); return err },
	"ArtifactReserveResponse":    func(b []byte) error { _, err := DecodeResponse[ArtifactReserveResponse](b); return err },
	"ArtifactCompleteResponse":   func(b []byte) error { _, err := DecodeResponse[ArtifactCompleteResponse](b); return err },
	"ResultResponse":             func(b []byte) error { _, err := DecodeResponse[ResultResponse](b); return err },
	"ContextV3":                  func(b []byte) error { _, err := DecodeResponse[ContextV3](b); return err },
	"AdmitResponseV3":            func(b []byte) error { _, err := DecodeResponse[AdmitResponseV3](b); return err },
	"BeginResponseV3":            func(b []byte) error { _, err := DecodeResponse[BeginResponseV3](b); return err },
	"ResourceContextV3":          func(b []byte) error { _, err := DecodeResponse[ResourceContextV3](b); return err },
	"ResourceAdmitResponseV3":    func(b []byte) error { _, err := DecodeResponse[ResourceAdmitResponseV3](b); return err },
	"ResourceTokenResponse":      func(b []byte) error { _, err := DecodeResponse[ResourceTokenResponse](b); return err },
	"PreparationReceiptResponse": func(b []byte) error { _, err := DecodeResponse[PreparationReceiptResponse](b); return err },
	"GitGrantResponse":           func(b []byte) error { _, err := DecodeResponse[GitGrantResponse](b); return err },
	"ErrorResponse":              func(b []byte) error { _, err := DecodeResponse[ErrorResponse](b); return err },
}

func codeOf(err error) string {
	if ce, ok := err.(*ContractError); ok {
		return string(ce.Code)
	}
	return "?"
}

func mustRead(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newCompiler(t testing.TB) (map[string]any, *jsonschema.Compiler) {
	t.Helper()
	c := jsonschema.NewCompiler()
	var v3 any
	for url, file := range map[string]string{
		schemaURLv1: "../v1/schema.json", schemaURLv2: "../v2/schema.json", schemaURLGit: "../../git/v1/schema.json", schemaURLv3: "schema.json",
	} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustRead(t, file)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(url, doc); err != nil {
			t.Fatal(err)
		}
		if url == schemaURLv3 {
			v3 = doc
		}
	}
	return v3.(map[string]any), c
}

func schemaVerdict(t testing.TB, c *jsonschema.Compiler, typeName string, payload []byte) bool {
	t.Helper()
	ref := schemaURLv3 + "#/$defs/" + typeName
	switch typeName {
	case "AdmitRequest", "TokenRequest", "SecretResolveRequest", "ArtifactReserveRequest", "ArtifactCompleteRequest", "TokenResponse", "SecretResolveResponse",
		"ArtifactReserveResponse", "ArtifactCompleteResponse", "ResultResponse":
		ref = schemaURLv1 + "#/$defs/" + typeName
	case "ResourceAdmitRequest", "ResourceTokenRequest", "ResourceTokenResponse", "PreparationReceiptResponse":
		ref = schemaURLv2 + "#/$defs/" + typeName
	}
	sch, err := c.Compile(ref)
	if err != nil {
		t.Fatalf("compile %s: %v", typeName, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	return sch.Validate(inst) == nil
}

func writeOrCompare(t *testing.T, rel string, body []byte) {
	t.Helper()
	if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(rel, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := mustRead(t, rel); !bytes.Equal(want, body) {
		t.Fatalf("%s is out of date; regenerate with KODAMA_UPDATE_GOLDEN=1", rel)
	}
}

func bodyOf(t testing.TB, c draftCase) []byte {
	t.Helper()
	if c.raw != nil {
		return []byte(*c.raw)
	}
	body, err := json.Marshal(c.payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// runCases runs the cases whose message type the filter selects through the Go
// decoder, requiring the fixture's verdict and code, and (for payloads) that the
// schema accepts everything the Go contract accepts.
func runCases(t *testing.T, selected func(typ string) bool) {
	t.Helper()
	_, compiler := newCompiler(t)
	ran := 0
	for _, c := range cases(t) {
		if !selected(c.typ) {
			continue
		}
		ran++
		body := bodyOf(t, c)
		t.Run(c.name, func(t *testing.T) {
			if len(c.reqs) == 0 {
				t.Fatal("fixture cites no requirement")
			}
			decode := decoders[c.typ]
			if decode == nil {
				t.Fatalf("no decoder for %s", c.typ)
			}
			err := decode(body)
			if (err == nil) != c.valid {
				t.Fatalf("Go contract: valid=%v, fixture expects %v (%v)", err == nil, c.valid, err)
			}
			if err != nil && codeOf(err) != c.code {
				t.Fatalf("code %q, want %q (%v)", codeOf(err), c.code, err)
			}
			if c.raw == nil && c.valid && !schemaVerdict(t, compiler, c.typ, body) {
				t.Fatalf("Go accepts a payload the schema rejects")
			}
		})
	}
	if ran == 0 {
		t.Fatal("no fixture selected")
	}
}

func contextTypes(typ string) bool {
	switch typ {
	case "ContextV3", "AdmitResponseV3", "BeginRequestV3", "BeginResponseV3":
		return true
	}
	return false
}

func resourceTypes(typ string) bool {
	switch typ {
	case "ResourceContextV3", "ResourceAdmitResponseV3", "ResourceAdmitRequest", "ResourceTokenRequest", "ResourceTokenResponse", "PreparationReceiptRequestV3", "PreparationReceiptResponse":
		return true
	}
	return false
}

func resultTypes(typ string) bool { return typ == "ResultV3" || typ == "ResultRequestV3" }

// Test_runtime_v3_contract_fixtures publishes the cases, requires the checked-in
// file to equal what the bases produce, and requires that every payload the Go
// contract accepts is accepted by the independent JSON Schema. It proves shapes
// only. PX-23, PX-24, PX-25, GR-06.
func Test_runtime_v3_contract_fixtures(t *testing.T) {
	_, compiler := newCompiler(t)
	var published []payloadCase
	seen := map[string]bool{}
	validTypes, invalidTypes := map[string]bool{}, map[string]bool{}
	for _, c := range cases(t) {
		if seen[c.name] {
			t.Fatalf("duplicate case %s", c.name)
		}
		seen[c.name] = true
		body := bodyOf(t, c)
		pc := payloadCase{Name: c.name, Type: c.typ, Requirements: c.reqs, Valid: c.valid, Code: c.code}
		if c.raw != nil {
			pc.Raw = c.raw
		} else {
			pc.Payload = body
			verdict := schemaVerdict(t, compiler, c.typ, body)
			pc.SchemaValid = &verdict
			if c.valid && !verdict {
				t.Errorf("%s: Go accepts a payload the schema rejects", c.name)
			}
		}
		published = append(published, pc)
		if c.valid {
			validTypes[c.typ] = true
		} else {
			invalidTypes[c.typ] = true
		}
	}
	out, err := json.MarshalIndent(published, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeOrCompare(t, "fixtures/cases.json", append(out, '\n'))

	for _, op := range Operations {
		for _, typeName := range append([]string{op.Request}, opResponses(op)...) {
			if typeName == "" {
				continue
			}
			if _, ok := decoders[typeName]; !ok {
				t.Errorf("%s names %s, which has no decoder", op.ID, typeName)
			}
			if strings.HasSuffix(typeName, "V3") {
				if op.Request == typeName && !invalidTypes[typeName] {
					t.Errorf("no negative fixture for %s", typeName)
				}
				if !validTypes[typeName] {
					t.Errorf("no positive fixture for %s", typeName)
				}
			}
		}
	}

	t.Run("traceability", func(t *testing.T) {
		covered := map[string]bool{}
		for _, c := range cases(t) {
			for _, id := range c.reqs {
				covered[id] = true
			}
		}
		for _, id := range []string{"PX-21", "PX-23", "PX-24", "PX-25", "GR-06", "WS-13", "WS-16", "DQ-16", "VS-14"} {
			if !covered[id] {
				t.Errorf("no fixture covers %s", id)
			}
		}
	})

	t.Run("the schema's error codes are exactly the codes the contract knows", func(t *testing.T) {
		s, _ := newCompiler(t)
		enum := s["$defs"].(map[string]any)["ErrorCode"].(map[string]any)["enum"].([]any)
		known := map[string]bool{}
		for _, c := range enum {
			code := domain.ErrorCode(c.(string))
			if _, ok := Status(code); !ok {
				t.Errorf("the schema lists %s, which Status does not know", code)
			}
			known[string(code)] = true
		}
		for _, op := range Operations {
			for _, code := range op.Errors {
				if !known[string(code)] {
					t.Errorf("%s may return %s, which the schema's error envelope does not accept", op.ID, code)
				}
			}
		}
	})

	t.Run("schema patterns equal the Go patterns", func(t *testing.T) {
		s, _ := newCompiler(t)
		defs := s["$defs"].(map[string]any)
		for name, pattern := range Patterns {
			if got := defs[name].(map[string]any)["pattern"]; got != pattern {
				t.Errorf("%s: schema pattern %v differs from Go pattern %s", name, got, pattern)
			}
		}
	})
}

func opResponses(op Operation) []string {
	var out []string
	for _, v := range op.Responses {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// sourceSetOf projects a decoded context to its source set.
func decodeContext(t testing.TB, ctx obj) ContextV3 {
	t.Helper()
	body, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeResponse[ContextV3](body)
	if err != nil {
		t.Fatalf("fixture context does not decode: %v", err)
	}
	return c
}

// Test_unit_runtime_context_source_set: the v3 context carries the run's origin,
// its source set (the Git contract's SourceSet with pinned commits and paths, and
// its digest) and the attempt's view of every source; the single source at "."
// is the single-repository case; the begin fence observes every source. The
// shared v1/v2 members keep their validators. PX-21, PX-23, PX-24, PX-25.
func Test_unit_runtime_context_source_set(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) { runCases(t, contextTypes) })

	t.Run("the source set projects to the Git contract's set and digest", func(t *testing.T) {
		ctx := decodeContext(t, ctxWriter())
		set, err := ctx.Workspace.SourceSet()
		if err != nil {
			t.Fatal(err)
		}
		if len(set.Sources) != 2 || set.Sources[0].Path != "backend" || set.Sources[0].Access != gitv1.AccessWrite || set.Sources[0].Commit != c40a ||
			set.Sources[0].Branch == nil || *set.Sources[0].Branch != taskWI || set.Sources[1].Branch != nil {
			t.Fatalf("projection: %+v", set)
		}
		digest, err := set.Digest()
		if err != nil || ctx.Workspace.SourceSetDigest == nil || string(*ctx.Workspace.SourceSetDigest) != digest {
			t.Fatalf("digest %s vs %v (%v)", digest, ctx.Workspace.SourceSetDigest, err)
		}
		// The set's own digest is independent of the attempt's view: a reader after
		// a writer sees another head but the same pinned set.
		reader := decodeContext(t, ctxReaderAfterWriter())
		if reader.Workspace.SourceSetDigest == nil || *reader.Workspace.SourceSetDigest != *ctx.Workspace.SourceSetDigest {
			t.Fatal("the pinned set does not change as the run's head moves")
		}
	})

	t.Run("one source at the root is the single-repository case", func(t *testing.T) {
		ctx := decodeContext(t, ctxPushReader())
		set, err := ctx.Workspace.SourceSet()
		if err != nil || len(set.Sources) != 1 || set.Sources[0].Path != gitv1.RootPath {
			t.Fatalf("%+v %v", set, err)
		}
		if ctx.Origin.Kind != OriginPush || ctx.WorkItem != nil || *ctx.Origin.PushID != "pu_01" || *ctx.Origin.Ref != "refs/heads/main" {
			t.Fatalf("origin %+v", ctx.Origin)
		}
	})

	t.Run("a source-free context has no sources and no digest", func(t *testing.T) {
		for _, ctx := range []obj{ctxProvision(), ctxFree()} {
			c := decodeContext(t, ctx)
			if len(c.Workspace.Sources) != 0 || c.Workspace.SourceSetDigest != nil || c.Workspace.Preparation != nil || c.Workspace.Access != AccessNone {
				t.Fatalf("%+v", c.Workspace)
			}
			if _, err := c.Workspace.SourceSet(); err == nil {
				t.Fatal("an empty set is not a valid source set")
			}
		}
	})

	t.Run("the agent namespace follows the origin", func(t *testing.T) {
		for name, tc := range map[string]struct {
			ctx  obj
			want string
		}{"work item": {ctxWriter(), agentWI}, "push": {ctxPushReader(), agentRun}, "manual": {ctxManualReader(), agentRun}, "provision": {ctxProvision(), agentRun}} {
			got, err := decodeContext(t, tc.ctx).AgentPattern()
			if err != nil || got != tc.want {
				t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
			}
		}
	})

	t.Run("the shared members keep the v1 and v2 validators", func(t *testing.T) {
		// A v3 context is the v2 context plus origin and git with another
		// workspace: the projection onto v2 must be valid for every valid case.
		for _, ctx := range []obj{ctxWriter(), ctxReaderAfterWriter(), ctxPushReader(), ctxManualReader(), ctxProvision(), ctxFree()} {
			c := decodeContext(t, ctx)
			if c.ContextVersion != 3 || c.RunModel != runtimev2.RunModel {
				t.Fatal("version or model")
			}
			shared := runtimev1.Context{TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID,
				ActionKey: c.ActionKey, Definition: c.Definition, WorkItem: c.WorkItem, Command: c.Command, Workspace: runtimev1.Workspace{Access: AccessNone},
				Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets, Limits: c.Limits}
			if err := shared.Validate(); err != nil {
				t.Fatalf("v1 validator: %v", err)
			}
		}
	})

	t.Run("begin derives the state over every source", func(t *testing.T) {
		writer := decodeContext(t, ctxWriter())
		reader := decodeContext(t, ctxReaderAfterWriter())
		free := decodeContext(t, ctxProvision())
		req := func(sources ...obj) BeginRequestV3 {
			if sources == nil {
				sources = []obj{}
			}
			body, _ := json.Marshal(obj{"sources": sources})
			r, err := DecodeRequest[BeginRequestV3](body)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		src := func(path, head string, clean, untracked, ignored bool) obj {
			return beginSource(path, head, clean, untracked, ignored)
		}
		for name, tc := range map[string]struct {
			ctx     ContextV3
			req     BeginRequestV3
			state   WorkspaceState
			ignored bool
			code    string
		}{
			"all clean":                    {writer, req(src("backend", c40a, true, false, false), src("frontend", c40b, true, false, false)), StateClean, false, ""},
			"ignored files never modify":   {writer, req(src("backend", c40a, true, false, true), src("frontend", c40b, true, false, false)), StateClean, true, ""},
			"one source dirty":             {writer, req(src("backend", c40a, true, false, false), src("frontend", c40b, false, false, false)), StateModified, false, ""},
			"one source with untracked":    {writer, req(src("backend", c40a, true, true, false), src("frontend", c40b, true, false, false)), StateModified, false, ""},
			"a source at another head":     {writer, req(src("backend", c40b, true, false, false), src("frontend", c40b, true, false, false)), StateModified, false, ""},
			"a reader sees the new head":   {reader, req(src("backend", c40c, true, false, false), src("frontend", c40b, true, false, false)), StateClean, false, ""},
			"a reader at the old head":     {reader, req(src("backend", c40a, true, false, false), src("frontend", c40b, true, false, false)), StateModified, false, ""},
			"a source missing":             {writer, req(src("backend", c40a, true, false, false)), "", false, "invalid_request"},
			"sources in the wrong order":   {writer, req(src("frontend", c40b, true, false, false), src("backend", c40a, true, false, false)), "", false, "invalid_request"},
			"a path of another set":        {writer, req(src("backend", c40a, true, false, false), src("web", c40b, true, false, false)), "", false, "invalid_request"},
			"source-free observes nothing": {free, req(), StateNone, false, ""},
			"source-free with a source":    {free, req(src("backend", c40a, true, false, false)), "", false, "invalid_request"},
		} {
			state, ignored, err := DeriveBegin(tc.ctx, tc.req)
			if tc.code != "" {
				if err == nil || codeOf(err) != tc.code {
					t.Errorf("%s: %v", name, err)
				}
				continue
			}
			if err != nil || state != tc.state || ignored != tc.ignored {
				t.Errorf("%s: got (%s, %v, %v)", name, state, ignored, err)
				continue
			}
			resp := BeginResponseV3{AttemptID: tc.ctx.AttemptID, BeganAt: "2026-10-30T10:05:00Z", WorkspaceState: state, IgnoredPresent: ignored}
			if err := CheckBeginResponse(tc.ctx, tc.req, resp); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			resp.WorkspaceState = StateClean
			if state != StateClean && CheckBeginResponse(tc.ctx, tc.req, resp) == nil {
				t.Errorf("%s: a response that is not the derivation must be refused", name)
			}
			resp.AttemptID = "att_other"
			if CheckBeginResponse(tc.ctx, tc.req, resp) == nil {
				t.Errorf("%s: a response for another attempt must be refused", name)
			}
		}
	})
}

// Test_unit_runtime_result_per_repository_outputs: the v3 result reports the
// refs an attempt updated per repository with the push record that made each,
// replacing sourceCommit; it is hashed with the unchanged canonicalization (the
// vectors were derived independently with Python); and the commit outputs are
// checked against the attempt's Git grant as the context records it. PX-10,
// PX-23, GR-06.
func Test_unit_runtime_result_per_repository_outputs(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) { runCases(t, resultTypes) })

	t.Run("canonical vectors derived independently", func(t *testing.T) {
		var vectors []struct {
			Name      string          `json:"name"`
			Result    json.RawMessage `json:"result"`
			Canonical string          `json:"canonical"`
			SHA256    string          `json:"sha256"`
		}
		if err := json.Unmarshal(mustRead(t, "fixtures/canonical.json"), &vectors); err != nil || len(vectors) < 5 {
			t.Fatalf("%d vectors: %v", len(vectors), err)
		}
		for _, v := range vectors {
			r, err := DecodeRequest[ResultV3](v.Result)
			if err != nil {
				t.Fatalf("%s: %v", v.Name, err)
			}
			canonical, err := CanonicalResult(r)
			if err != nil || string(canonical) != v.Canonical {
				t.Errorf("%s: canonical bytes\n got: %s\nwant: %s (%v)", v.Name, canonical, v.Canonical, err)
			}
			if got, err := ResultHash(r); err != nil || string(got) != v.SHA256 {
				t.Errorf("%s: hash %s, want %s (%v)", v.Name, got, v.SHA256, err)
			}
			if !bytes.Contains(canonical, []byte(`"commits":[`)) || bytes.Contains(canonical, []byte("sourceCommit")) {
				t.Errorf("%s: the canonical form carries commits and no sourceCommit", v.Name)
			}
		}
	})

	t.Run("a result has one hash whatever the spelling of its commits", func(t *testing.T) {
		a := ResultV3{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: []CommitOutput{{RepositoryID: "repo_01", Ref: taskWI, Commit: c40c, PushID: "pu_07"}},
			Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		b := a
		b.Commits = []CommitOutput{{RepositoryID: "repo_01", Ref: taskWI, Commit: c40c, PushID: "pu_08"}}
		ha, _ := ResultHash(a)
		hb, _ := ResultHash(b)
		if ha == "" || ha == hb {
			t.Fatal("the push reference is part of the hash")
		}
		unordered := a
		unordered.Commits = []CommitOutput{{RepositoryID: "repo_02", Ref: taskWI, Commit: c40c, PushID: "pu_07"}, {RepositoryID: "repo_01", Ref: taskWI, Commit: c40c, PushID: "pu_08"}}
		if _, err := ResultHash(unordered); err == nil {
			t.Fatal("unordered commits must be refused, not normalized")
		}
	})

	t.Run("commit outputs are checked against the attempt's grant", func(t *testing.T) {
		writer := decodeContext(t, ctxWriter())
		reader := decodeContext(t, ctxReaderAfterWriter())
		push := decodeContext(t, ctxPushWriter())
		free := decodeContext(t, ctxProvision())
		artifacts := map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: writer.Scope(), Attempt: writer.AttemptID, Kind: runtimev1.KindFile, Usable: true}}
		readerArtifacts := map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: reader.Scope(), Attempt: reader.AttemptID, Kind: runtimev1.KindFile, Usable: true}}
		pushArtifacts := map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: push.Scope(), Attempt: push.AttemptID, Kind: runtimev1.KindFile, Usable: true}}
		freeArtifacts := map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: free.Scope(), Attempt: free.AttemptID, Kind: runtimev1.KindFile, Usable: true}}
		agentNotes := "refs/heads/kodama/agents/wi_1042/run_01/notes"
		result := func(outs ...CommitOutput) ResultV3 {
			r := ResultV3{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: append([]CommitOutput{}, outs...), Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
			r.Outputs["report"] = runtimev1.StringValue("art_0001")
			return r
		}
		for name, tc := range map[string]struct {
			r    ResultV3
			ctx  ContextV3
			arts map[domain.ArtifactID]runtimev1.ArtifactRef
			code domain.ErrorCode
		}{
			"writer task branch":                {result(CommitOutput{"repo_01", taskWI, c40c, "pu_07"}), writer, artifacts, ""},
			"writer agent branch":               {result(CommitOutput{"repo_01", agentNotes, c40c, "pu_07"}), writer, artifacts, ""},
			"writer no commits":                 {result(), writer, artifacts, ""},
			"writer in a read repo":             {result(CommitOutput{"repo_02", agentNotes, c40c, "pu_07"}), writer, artifacts, ""},
			"writer task branch of a read repo": {result(CommitOutput{"repo_02", taskWI, c40c, "pu_07"}), writer, artifacts, runtimev1.CodeOutputInvalid},
			"reader task branch":                {result(CommitOutput{"repo_01", taskWI, c40d, "pu_08"}), reader, readerArtifacts, runtimev1.CodeOutputInvalid},
			"reader agent branch":               {result(CommitOutput{"repo_01", agentNotes, c40d, "pu_08"}), reader, readerArtifacts, ""},
			"reader default branch":             {result(CommitOutput{"repo_01", "refs/heads/main", c40d, "pu_08"}), reader, readerArtifacts, runtimev1.CodeOutputInvalid},
			"another run's agent branch":        {result(CommitOutput{"repo_01", "refs/heads/kodama/agents/wi_1042/run_02/x", c40d, "pu_08"}), writer, artifacts, runtimev1.CodeOutputInvalid},
			"another work item's task":          {result(CommitOutput{"repo_01", "refs/heads/kodama/work/wi_9999", c40d, "pu_08"}), writer, artifacts, runtimev1.CodeOutputInvalid},
			"unknown repository":                {result(CommitOutput{"repo_99", agentNotes, c40d, "pu_08"}), writer, artifacts, runtimev1.CodeOutputInvalid},
			"push-origin run branch":            {result(CommitOutput{"repo_01", runOut, c40d, "pu_08"}), push, pushArtifacts, ""},
			"push-origin agent pattern":         {result(CommitOutput{"repo_01", agentNotes, c40d, "pu_08"}), push, pushArtifacts, runtimev1.CodeOutputInvalid},
			"no grant at all":                   {result(CommitOutput{"repo_01", runOut, c40d, "pu_08"}), free, freeArtifacts, runtimev1.CodeOutputInvalid},
			"missing declared output":           {ResultV3{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: []CommitOutput{}, Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}, writer, artifacts, runtimev1.CodeOutputInvalid},
			"foreign artifact":                  {result(), writer, map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: domain.Scope{TenantID: "t_other", ProjectID: "p_x"}, Attempt: writer.AttemptID, Kind: runtimev1.KindFile, Usable: true}}, domain.CodeNotFound},
			"invalid result":                    {ResultV3{Outcome: OutcomeCompleted}, writer, artifacts, domain.CodeInvalidRequest},
		} {
			err := CheckResult(tc.r, tc.ctx, tc.arts)
			switch {
			case tc.code == "" && err != nil:
				t.Errorf("%s: %v", name, err)
			case tc.code != "" && (err == nil || codeOf(err) != string(tc.code)):
				t.Errorf("%s: got %v, want %s", name, err, tc.code)
			}
		}
	})

	t.Run("there is no source proposal route", func(t *testing.T) {
		for _, op := range Operations {
			if op.Request == "SourceProposalRequest" || op.ID == "proposeSource" || strings.Contains(op.Path, "/source") {
				t.Fatalf("%s: an internally hosted source is pushed, not proposed", op.ID)
			}
		}
		ids := map[string]bool{}
		for _, op := range Operations {
			ids[op.ID] = true
		}
		for _, id := range []string{"admit", "renewToken", "getContext", "begin", "resolveSecret", "reserveArtifact", "completeArtifact", "submitResult"} {
			if !ids[id] {
				t.Errorf("missing operation %s", id)
			}
		}
		for _, op := range Operations {
			for _, code := range op.Errors {
				if _, ok := Status(code); !ok {
					t.Errorf("%s lists unknown code %s", op.ID, code)
				}
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

// frozenTrees are the SHA-256 of every contract file of an earlier runtime
// version (the relative path and the content digest of each non-Go file, in
// path order). A change to any schema, fixture or vector of /runtime/v1 or
// /runtime/v2 changes the value.
var frozenTrees = map[string]string{
	"../v1": "90760360885bf8f14ca9dfca47228b30da4b07523b90a2a1afe2d2792c768c38",
	"../v2": "8528c4431b20445cd042be125fd3c4dfff0d24a4794d274149c4397ed0a218ff",
}

func treeDigest(t testing.TB, root string) string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		sum := sha256.Sum256(mustRead(t, filepath.Join(root, rel)))
		h.Write([]byte(rel + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Test_unit_runtime_old_hashes_unchanged: adding /runtime/v3 changes nothing of
// /runtime/v1 or /runtime/v2: their schemas, fixtures and canonical vectors are
// byte-identical, every historical result hash still verifies under the v1 and
// v2 hash functions, and a v3 result never collides with a v1 result because the
// two canonical forms differ in their members. PX-21.
func Test_unit_runtime_old_hashes_unchanged(t *testing.T) {
	for root, want := range frozenTrees {
		if got := treeDigest(t, root); got != want {
			t.Errorf("%s: the frozen contract files changed (tree digest %s)", root, got)
		}
	}

	var v1cases []payloadCase
	if err := json.Unmarshal(mustRead(t, "../v1/fixtures/cases.json"), &v1cases); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range v1cases {
		if c.Type != "ResultRequest" || !c.Valid || c.Raw != nil {
			continue
		}
		req, err := runtimev1.DecodeRequest[runtimev1.ResultRequest](c.Payload)
		if err != nil {
			t.Fatalf("%s: v1 result fixture no longer decodes: %v", c.Name, err)
		}
		got1, err1 := runtimev1.ResultHash(req.Result)
		got2, err2 := runtimev2.ResultHash(req.Result)
		if err1 != nil || err2 != nil || got1 != req.ResultHash || got2 != req.ResultHash {
			t.Fatalf("%s: historical hash %s, v1 %s v2 %s", c.Name, req.ResultHash, got1, got2)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("only %d v1 result vectors checked", checked)
	}

	var vectors []struct {
		Name      string `json:"name"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		Error     bool   `json:"error"`
	}
	if err := json.Unmarshal(mustRead(t, "../v1/fixtures/canonical.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		got, err := runtimev1.Canonicalize([]byte(v.Input))
		if v.Error != (err != nil) || (!v.Error && string(got) != v.Canonical) {
			t.Errorf("%s: canonical vector changed", v.Name)
		}
	}

	t.Run("a v3 result is not a v1 result", func(t *testing.T) {
		v3 := ResultV3{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: []CommitOutput{}, Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		v1 := runtimev1.Result{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h3, err3 := ResultHash(v3)
		h1, err1 := runtimev1.ResultHash(v1)
		if err3 != nil || err1 != nil || h3 == h1 {
			t.Fatalf("v3 %s v1 %s (%v %v)", h3, h1, err3, err1)
		}
		// The v3 decoder refuses a v1 result and the reverse.
		body, _ := json.Marshal(v1)
		if _, err := DecodeRequest[ResultV3](body); err == nil {
			t.Fatal("a v1 result is not a v3 result")
		}
		body, _ = json.Marshal(v3)
		if _, err := runtimev1.DecodeRequest[runtimev1.Result](body); err == nil {
			t.Fatal("a v3 result is not a v1 result")
		}
	})

	t.Run("the earlier operation tables and schemas keep their shape", func(t *testing.T) {
		if len(runtimev1.Operations) != 8 {
			t.Fatalf("v1 has %d operations", len(runtimev1.Operations))
		}
		if len(runtimev2.Operations) != 14 {
			t.Fatalf("v2 has %d operations", len(runtimev2.Operations))
		}
		if runtimev2.ContextVersion != 2 || ContextVersion != 3 {
			t.Fatal("context versions")
		}
	})
}

// Test_unit_runtime_git_grant_delivery_shape: the context tells the entrypoint
// where the attempt's Git credential file and CA are and which refs the grant
// allows, and never carries a secret. The delivery's access is exactly the
// attempt grant's repository claims, and those claims permit the agent
// namespace everywhere and the writer's task branch only for the attempt that
// holds the writer reservation. GR-06, PX-23.
func Test_unit_runtime_git_grant_delivery_shape(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) {
		_, compiler := newCompiler(t)
		for _, c := range cases(t) {
			if c.typ != "ContextV3" || !strings.Contains(c.name, "git") && !strings.Contains(c.name, "grant") && !strings.Contains(c.name, "credential") && !strings.Contains(c.name, "ca_file") {
				continue
			}
			body := bodyOf(t, c)
			t.Run(c.name, func(t *testing.T) {
				err := decoders[c.typ](body)
				if (err == nil) != c.valid {
					t.Fatalf("valid=%v, fixture expects %v (%v)", err == nil, c.valid, err)
				}
				if c.valid && !schemaVerdict(t, compiler, c.typ, body) {
					t.Fatal("the schema rejects it")
				}
			})
		}
	})

	t.Run("the descriptor names where, never what", func(t *testing.T) {
		var members []string
		typ := reflect.TypeOf(GitDelivery{})
		for i := 0; i < typ.NumField(); i++ {
			members = append(members, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		want := []string{"grantId", "baseUrl", "credentialFile", "caFile", "repositories", "runtimeGrants"}
		if !reflect.DeepEqual(members, want) {
			t.Fatalf("members %v, want %v", members, want)
		}
		body, _ := json.Marshal(ctxWriter())
		for _, forbidden := range []string{"password", "kg_", "BEGIN CERTIFICATE", "BEGIN PRIVATE"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("the context mentions %q", forbidden)
			}
		}
		ctx := decodeContext(t, ctxWriter())
		if ctx.Git.CredentialFile == nil || *ctx.Git.CredentialFile != GitDirectory+"/credential" || !strings.HasPrefix(ctx.Git.BaseURL, "https://") || ctx.Git.CAFile != nil {
			t.Fatalf("%+v", ctx.Git)
		}
		if GitUsername != "kodama" || MountPath != "/workspace" {
			t.Fatal("constants")
		}
	})

	t.Run("the delivery is the grant's repository claims", func(t *testing.T) {
		for name, tc := range map[string]struct {
			ctx obj
			// permitted and refused updates by repository
			permit, refuse map[string][]string
		}{
			"work-item writer": {ctxWriter(),
				map[string][]string{"repo_01": {taskWI, "refs/heads/kodama/agents/wi_1042/run_01/notes"}, "repo_02": {"refs/heads/kodama/agents/wi_1042/run_01/x"}},
				map[string][]string{"repo_01": {"refs/heads/main", "refs/heads/kodama/work/wi_9999", "refs/heads/kodama/agents/wi_1042/run_02/x", "refs/kodama/runs/run_01/1", "refs/tags/v1"}, "repo_02": {taskWI, "refs/heads/main"}, "repo_99": {taskWI}}},
			"work-item reader": {ctxReaderAfterWriter(),
				map[string][]string{"repo_01": {"refs/heads/kodama/agents/wi_1042/run_01/notes"}, "repo_02": {"refs/heads/kodama/agents/wi_1042/run_01/x"}},
				map[string][]string{"repo_01": {taskWI, "refs/heads/main"}, "repo_02": {taskWI}}},
			"push-origin writer": {ctxPushWriter(),
				map[string][]string{"repo_01": {runOut, "refs/heads/kodama/runs/run_01/other"}},
				map[string][]string{"repo_01": {"refs/heads/main", taskWI, "refs/heads/kodama/runs/run_02/out"}}},
		} {
			ctx := decodeContext(t, tc.ctx)
			claims := gitv1.GrantClaims{
				GrantID: *ctx.Git.GrantID, Audience: gitv1.GrantAudience, TenantID: string(ctx.TenantID), ProjectID: string(ctx.ProjectID),
				Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: string(ctx.AttemptID), RunID: ptr(string(ctx.RunID)), AttemptID: ptr(string(ctx.AttemptID))},
				Kind:         gitv1.GrantAttempt,
				Repositories: ctx.Git.GrantRepositories(),
				IssuedAt:     "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T11:00:00Z",
			}
			if err := claims.Validate(); err != nil {
				t.Fatalf("%s: the delivery is not a valid attempt grant: %v", name, err)
			}
			if err := claims.CheckScope(string(ctx.TenantID), string(ctx.ProjectID), ctx.Git.Repositories[0].RepositoryID); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for repo, refs := range tc.permit {
				for _, ref := range refs {
					if !claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: c40a, New: c40b}) {
						t.Errorf("%s: %s %s must be permitted", name, repo, ref)
					}
				}
			}
			for repo, refs := range tc.refuse {
				for _, ref := range refs {
					if claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: c40a, New: c40b}) {
						t.Errorf("%s: %s %s must be refused", name, repo, ref)
					}
				}
			}
			// Everything the delivery lists is a valid grant pattern and is never
			// a human ceiling, a server ref or the default branch.
			for _, r := range ctx.Git.Repositories {
				for _, p := range r.Update {
					if err := gitv1.ValidateRefPattern(p); err != nil || p == "refs/heads/*" || strings.HasPrefix(p, gitv1.ServerRefPrefix) {
						t.Errorf("%s: pattern %q", name, p)
					}
				}
			}
		}
	})

	t.Run("an attempt's access is derived, not chosen", func(t *testing.T) {
		writer := decodeContext(t, ctxWriter())
		derived, err := writer.GitAccess()
		if err != nil || !reflect.DeepEqual(derived, writer.Git.Repositories) {
			t.Fatalf("%+v %v", derived, err)
		}
		// The same run's reader derives a narrower grant from the same set.
		reader := writer
		reader.Workspace.Access = AccessRead
		derived, err = reader.GitAccess()
		if err != nil || len(derived[0].Update) != 1 {
			t.Fatalf("reader %+v %v", derived, err)
		}
		// Two write sources of one repository list their branches sorted and once.
		two := writer
		src := two.Workspace.Sources[0]
		other := src
		other.Path = "backend2"
		b := "refs/heads/kodama/agents/wi_1042/run_01/side"
		other.Branch = &b
		dup := src
		dup.Path = "backend3"
		two.Workspace.Sources = []SourceV3{src, other, dup}
		derived, err = two.GitAccess()
		if err != nil || !reflect.DeepEqual(derived[0].Update, []string{agentWI, b, taskWI}) {
			t.Fatalf("%+v %v", derived, err)
		}
	})
}

// Test_unit_runtime_v3_preparation_source_set: the v3 resource subjects carry one
// preparation for the whole source set with a read-only Git delivery and a
// per-source receipt, and reuse the v2 service and token messages unchanged.
// PX-21, PX-23, WS-13, GR-06, VS-14.
func Test_unit_runtime_v3_preparation_source_set(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) { runCases(t, resourceTypes) })

	prep := func(t *testing.T, o obj) ResourceContextV3 {
		t.Helper()
		body, _ := json.Marshal(o)
		c, err := DecodeResponse[ResourceContextV3](body)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("the preparation is the attempt's source set", func(t *testing.T) {
		attempt := decodeContext(t, ctxWriter())
		p := prep(t, prepCtx())
		if err := CheckPreparationMatches(p, attempt); err != nil {
			t.Fatal(err)
		}
		for name, edit := range map[string]func(*ResourceContextV3){
			"another commit": func(c *ResourceContextV3) { c.Preparation.Sources[0].Commit = c40d },
			"another path":   func(c *ResourceContextV3) { c.Preparation.Sources[1].Path = "web" },
			"another URL": func(c *ResourceContextV3) {
				c.Preparation.Sources[0].RepositoryURL = gitBase + "/t_acme/p_inventory/other.git"
			},
			"a missing source": func(c *ResourceContextV3) { c.Preparation.Sources = c.Preparation.Sources[:1] },
			"another service":  func(c *ResourceContextV3) { c.Preparation.Git.BaseURL = "https://other.example.invalid/git" },
			"another run":      func(c *ResourceContextV3) { c.RunID = "run_02" },
		} {
			c := prep(t, prepCtx())
			c.Preparation.Sources = append([]PreparationSource{}, c.Preparation.Sources...)
			edit(&c)
			if CheckPreparationMatches(c, attempt) == nil {
				t.Errorf("%s must be refused", name)
			}
		}
		if err := CheckPreparationMatches(prep(t, svcCtx()), attempt); err == nil {
			t.Error("a service is not a preparation")
		}
		// The preparation context is derived from the attempt's sources.
		if got := attempt.Workspace.SourcePreparation(); !reflect.DeepEqual(got, p.Preparation.Sources) {
			t.Errorf("%+v", got)
		}
	})

	t.Run("the preparation's Git delivery is a read-only preparation grant", func(t *testing.T) {
		p := prep(t, prepCtx())
		claims := gitv1.GrantClaims{
			GrantID: p.Preparation.Git.GrantID, Audience: gitv1.GrantAudience, TenantID: string(p.TenantID), ProjectID: string(p.ProjectID),
			Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: "prep_" + string(p.RunID), RunID: ptr(string(p.RunID))},
			Kind:         gitv1.GrantPreparation,
			Repositories: p.Preparation.Git.GrantRepositories(),
			IssuedAt:     "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T11:00:00Z",
		}
		if err := claims.Validate(); err != nil {
			t.Fatalf("not a valid preparation grant: %v", err)
		}
		for _, repo := range []string{"repo_01", "repo_02"} {
			for _, ref := range []string{"refs/heads/main", taskWI, agentWI[:len(agentWI)-1] + "x", runOut} {
				if claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: c40a, New: c40b}) {
					t.Errorf("a preparation grant must update nothing: %s %s", repo, ref)
				}
			}
		}
		typ := reflect.TypeOf(PreparationGit{})
		var members []string
		for i := 0; i < typ.NumField(); i++ {
			members = append(members, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		if !reflect.DeepEqual(members, []string{"grantId", "baseUrl", "credentialFile", "caFile", "repositories"}) {
			t.Fatalf("members %v", members)
		}
	})

	t.Run("a receipt covers every source at its pinned commit", func(t *testing.T) {
		p := prep(t, prepCtx())
		decodeReq := func(o obj) PreparationReceiptRequestV3 {
			body, _ := json.Marshal(o)
			r, err := DecodeRequest[PreparationReceiptRequestV3](body)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		for name, tc := range map[string]struct {
			req  obj
			ctx  ResourceContextV3
			code domain.ErrorCode
		}{
			"every source":         {receiptReq(), p, ""},
			"within the tries":     {mut(t, receiptReq(), "attempts", 2), p, ""},
			"too many tries":       {mut(t, receiptReq(), "attempts", 3), p, domain.CodeInvalidRequest},
			"a source missing":     {mut(t, receiptReq(), "sources", []any{receiptSource("backend", c40a)}), p, domain.CodeInvalidRequest},
			"an extra source":      {mut(t, receiptReq(), "sources", []any{receiptSource("backend", c40a), receiptSource("frontend", c40b), receiptSource("x", c40c)}), p, domain.CodeInvalidRequest},
			"sources out of order": {mut(t, receiptReq(), "sources", []any{receiptSource("frontend", c40b), receiptSource("backend", c40a)}), p, domain.CodeInvalidRequest},
			"another commit":       {mut(t, receiptReq(), "sources.1.commit", c40d), p, domain.CodeInvalidRequest},
			"a service resource":   {receiptReq(), prep(t, svcCtx()), domain.CodeForbidden},
		} {
			err := CheckReceipt(tc.ctx, decodeReq(tc.req))
			switch {
			case tc.code == "" && err != nil:
				t.Errorf("%s: %v", name, err)
			case tc.code != "" && (err == nil || codeOf(err) != string(tc.code)):
				t.Errorf("%s: got %v, want %s", name, err, tc.code)
			}
		}
	})

	t.Run("the resource routes", func(t *testing.T) {
		byID := map[string]Operation{}
		for _, op := range Operations {
			if op.Subject == SubjectResource {
				byID[op.ID] = op
			}
		}
		if len(byID) != 5 {
			t.Fatalf("resource operations: %d", len(byID))
		}
		if byID["completePreparation"].Request != "PreparationReceiptRequestV3" || byID["getResourceContext"].Responses[200] != "ResourceContextV3" || byID["admitResource"].Responses[200] != "ResourceAdmitResponseV3" {
			t.Fatal("the resource routes carry the v3 messages")
		}
		// Services reuse the v2 messages: the secrets route is the v2 one.
		if byID["resolveResourceSecret"].Request != "SecretResolveRequest" {
			t.Fatal("the service secret route is unchanged")
		}
		if err := runtimev2.CheckResourceCall(byID["completePreparation"], ResourceService); err == nil {
			t.Fatal("a service has no preparation route")
		}
		if err := runtimev2.CheckResourceCall(byID["resolveResourceSecret"], ResourcePreparation); err == nil {
			t.Fatal("a preparation has no secrets route")
		}
		if err := runtimev2.CheckAudience(SubjectResource, "kodama-runtime"); err == nil || runtimev2.CheckAudience(SubjectResource, ResourceAudience) != nil {
			t.Fatal("audiences are unchanged")
		}
		if ResourceAudience != "kodama-runtime-resource" || !strings.HasPrefix(ResourceBasePath, "/runtime/v3/") {
			t.Fatal("constants")
		}
	})
}
