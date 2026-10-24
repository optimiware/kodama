package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

var scopeA = executiontest.ScopeA

type (
	httpWriter  = http.ResponseWriter
	httpRequest = http.Request
)

// newWorld is a configured project: the model secret exists and the settings
// are stored, so attempts of scopeA can ask for models.
func newWorld(t *testing.T, edit ...func(*gateway.SettingsEdit)) *gatewaytest.Env {
	t.Helper()
	e := gatewaytest.New(t)
	e.ModelSecret(scopeA)
	ed := e.Edit()
	for _, f := range edit {
		f(&ed)
	}
	e.Configure(e.Replica, scopeA, ed)
	return e
}

func body(prompt string) []byte { return gatewaytest.Body("gpt-a", prompt, 64) }

func reqState(t *testing.T, e *gatewaytest.Env, run domain.RunID, ordinal int) [2]string {
	t.Helper()
	rs := e.Requests(run)
	if ordinal > len(rs) {
		t.Fatalf("request %d does not exist: %v", ordinal, rs)
	}
	return rs[ordinal-1]
}

// failingPins is a pin registrar whose database write always fails, so the
// first admission of a run rolls back.
type failingPins struct{}

func (failingPins) Register(context.Context, pgx.Tx, domain.Scope, domain.RunID, gateway.CredentialRef) error {
	return gatewaytest.ErrInjected
}
