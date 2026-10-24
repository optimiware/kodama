//go:build windows

package auth

import "context"

// PromptFree: the Windows Credential Manager never prompts for the current
// user's credentials.
func (systemKeyring) PromptFree(context.Context) error { return nil }
