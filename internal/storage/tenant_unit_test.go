package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
)

type neverBegins struct{ called bool }

func (n *neverBegins) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	n.called = true
	return nil, errors.New("must not be called")
}

func TestWithTenantRejectsInvalidTenantBeforeUsingDatabase(t *testing.T) {
	for _, id := range []domain.TenantID{"", "has space", "x'; DROP TABLE tenants;--", "-lead", domain.TenantID(make([]byte, 129))} {
		db := &neverBegins{}
		called := false
		err := WithTenant(context.Background(), db, id, func(context.Context, pgx.Tx) error { called = true; return nil })
		if !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("tenant %q: err = %v, want domain.ErrInvalidValue", id, err)
		}
		if db.called || called {
			t.Errorf("tenant %q: database or fn was used", id)
		}
	}
}

func TestWithTenantRequiresDependencies(t *testing.T) {
	if err := WithTenant(context.Background(), nil, "t1", func(context.Context, pgx.Tx) error { return nil }); err == nil {
		t.Error("nil database accepted")
	}
	if err := WithTenant(context.Background(), &neverBegins{}, "t1", nil); err == nil {
		t.Error("nil fn accepted")
	}
}

func TestOpenRequiresURL(t *testing.T) {
	if _, err := Open(context.Background(), Config{}); err == nil {
		t.Error("empty URL accepted")
	}
	if _, err := Open(context.Background(), Config{URL: "postgres://u:secretpw@h:notaport/db"}); err == nil {
		t.Error("bad URL accepted")
	} else if strings.Contains(err.Error(), "secretpw") {
		t.Errorf("error leaks the password: %v", err)
	}
}
