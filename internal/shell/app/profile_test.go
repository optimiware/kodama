package app

import (
	"context"
	"testing"
	"time"
)

func TestProfileTestRequiresAndAcceptsFullyInjectedFakes(t *testing.T) {
	config := fullTestProfileConfig(t)
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New ProfileTest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
