//go:build !unit

package layer_test

import (
	"testing"

	"github.com/optimiware/kodama/test/support/layer"
)

func TestCompleteLayerRunsServices(t *testing.T) {
	if layer.Unit {
		t.Fatal("layer.Unit must be false without the unit tag")
	}
	layer.SkipService(t, "PostgreSQL") // must not skip
}
