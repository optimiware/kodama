//go:build unit

package layer_test

import (
	"testing"

	"github.com/optimiware/kodama/test/support/layer"
)

func TestUnitTagSkipsServices(t *testing.T) {
	if !layer.Unit {
		t.Fatal("unit tag must set layer.Unit")
	}
	t.Run("skipped", func(t *testing.T) {
		layer.SkipService(t, "PostgreSQL")
		t.Fatal("SkipService must skip in the unit layer")
	})
}
