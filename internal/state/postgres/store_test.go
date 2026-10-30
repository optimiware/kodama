package postgres

import (
	"testing"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
)

func TestEmptyArtifactReferencesEncodeAsJSONArray(t *testing.T) {
	if got := string(referencesJSON(nil)); got != "[]" {
		t.Fatalf("nil references encoded as %s", got)
	}
	if got := string(referencesJSON([]statev1.ArtifactReference{})); got != "[]" {
		t.Fatalf("empty references encoded as %s", got)
	}
}
