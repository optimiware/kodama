package integration_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
)

// TestGateContract (SD-13, SD-A11): the PostgreSQL gate store, the real
// identity authority and the real execution module pass the same contract the
// memory adapter passes in the unit layer. It skips in the unit layer.
func TestGateContract(t *testing.T) {
	gatescontract.Contract(t, func(t testing.TB) gatescontract.Harness {
		return gatestest.NewPostgresHarness(gatestest.New(t))
	})
}
