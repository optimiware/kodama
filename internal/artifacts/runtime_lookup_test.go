package artifacts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
)

func TestRuntimeMetadataOnlyReadsPinnedInputs(t *testing.T) {
	data := []byte("verified input")
	env, caller := setup(t, artifactstest.Options{})
	id := env.ReserveAndUpload(caller, attempt, "bundle", artifacts.RoleOutput, data)
	if _, err := env.Complete(caller, attempt, id, data); err != nil {
		t.Fatal(err)
	}
	digest := artifacts.DigestOf(data)

	env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
		g.Inputs = []artifacts.InputRef{{ID: id, Digest: digest}}
	})
	v, err := env.Service.RuntimeMetadata(env.Ctx(), caller, attempt, id)
	if err != nil || v.Download == nil || v.Artifact.ID() != id {
		t.Fatalf("runtime metadata: %+v %v", v, err)
	}

	env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
		g.Inputs = nil
	})
	if _, err := env.Service.RuntimeMetadata(env.Ctx(), caller, attempt, id); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("unpinned artifact: %v", err)
	}

	env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
		g.Inputs = []artifacts.InputRef{{ID: id, Digest: artifacts.Digest("sha256:" + strings.Repeat("0", 64))}}
	})
	if _, err := env.Service.RuntimeMetadata(env.Ctx(), caller, attempt, id); !errors.Is(err, artifacts.ErrIncomplete) {
		t.Fatalf("digest mismatch: %v", err)
	}
}
