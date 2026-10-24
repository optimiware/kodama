//go:build !linux && !windows

package auth

import "context"

// PromptFree cannot prove that the platform keyring will not prompt (for
// example a locked macOS keychain raises a dialog), so a non-interactive
// invocation refuses it rather than risk blocking. Use --credential-store file.
func (systemKeyring) PromptFree(context.Context) error { return ErrInteractionRequired }
