//go:build linux

package auth

import "context"

// PromptFree refuses noninteractive use of the Linux Secret Service backend.
// go-keyring v0.2.8 calls Unlock on the collection and again on the item, and
// Delete follows a provider prompt. An unlocked collection is not proof that
// the following Get, Set, or Delete will not call org.freedesktop.Secret.Prompt,
// and the collection can lock between a probe and the operation. There is no
// operation-level switch that disables prompting, so a noninteractive
// invocation must fail closed. Use --credential-store file.
func (systemKeyring) PromptFree(context.Context) error { return ErrInteractionRequired }
