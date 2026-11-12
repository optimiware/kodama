//go:build integration

package native_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// TestNativeGitMeetsDeclaredMinimum proves the actual host executable that the
// service runs satisfies the product's declared minimum. It is a required
// integration assertion: an old or missing Git fails this test instead of
// skipping, because the declared boundary is part of the served behavior.
func TestNativeGitMeetsDeclaredMinimum(t *testing.T) {
	executor, err := app.NewExecutor(app.ExecutorConfig{})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if err := native.RequireGitVersion(context.Background(), executor); err != nil {
		t.Fatalf("native Git does not meet the declared minimum %s: %v", native.MinimumGitVersion, err)
	}
}
