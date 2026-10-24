package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	scopeA  = artifactstest.ScopeA
	scopeA2 = artifactstest.ScopeA2
	scopeB  = artifactstest.ScopeB
	bigOK   = artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 100, DiagnosticBytes: 1 << 20, DiagnosticFiles: 100, BundleBytes: 1 << 20, BundleFiles: 10}
)

func at() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// fileArtifact builds a reserved file artifact for the attempt.
func fileArtifact(t testing.TB, scope domain.Scope, attempt domain.AttemptID, name string, data []byte, role artifacts.Role) artifacts.Artifact {
	t.Helper()
	size := int64(len(data))
	id := artifacts.RandomIDs{}.NewArtifactID()
	res := artifacts.Reservation{Scope: scope, Attempt: attempt, Run: "run_1", Step: "build", Name: name, Kind: artifacts.KindFile, Role: role,
		Digest: artifacts.DigestOf(data), MediaType: "text/plain", Size: &size, Provenance: artifactstest.Source}
	now := at()
	a, err := artifacts.NewFile(id, res, artifacts.ObjectKey(scope, id, artifacts.SegmentStaging, fmt.Sprintf("n%d", now.UnixNano())), now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func reserveCmd(a artifacts.Artifact, lim artifacts.Limits) artifacts.ReserveCommand {
	return artifacts.ReserveCommand{Artifact: a, Limits: lim, Actor: artifacts.Actor{Type: "workload", ID: string(a.Attempt())}, RequestID: "req_test", Now: at()}
}
