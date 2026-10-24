package artifactstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresArtifactsContract runs the same persistence contract on the
// PostgreSQL adapter. It is recorded in the `test` environment (T-026) and skips
// in the unit layer and without PostgreSQL.
func TestPostgresArtifactsContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	artifactstest.RepoContract(t, artifactstest.OpenPostgresRepo)
}

// TestS3ArtifactObjectsContract runs the same object-store contract on the S3
// adapter in the `test` environment.
func TestS3ArtifactObjectsContract(t *testing.T) {
	layer.SkipService(t, "S3")
	artifactstest.ObjectContract(t, artifactstest.OpenS3Objects)
}
