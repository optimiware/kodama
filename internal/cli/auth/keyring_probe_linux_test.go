//go:build linux

package auth

import (
	"context"
	"errors"
	"testing"
)

func Test_linux_keyring_probe_never_claims_prompt_free(t *testing.T) {
	err := (systemKeyring{}).PromptFree(context.Background())
	if !errors.Is(err, ErrInteractionRequired) {
		t.Fatalf("PromptFree() = %v, want ErrInteractionRequired", err)
	}
}
