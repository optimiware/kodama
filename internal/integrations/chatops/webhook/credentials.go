package webhook

import (
	"context"
	"errors"
	"strings"

	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// resolveCredential obtains the authentication material of one send, bound to
// the recipient of the delivery's verified binding. It returns a header value
// ("" for a credential-free destination) or the classified outcome to record.
//
// Failure is closed: a destination that names a credential and has no source, a
// reference the source refuses (missing, revoked, foreign, wrong recipient) or
// material that cannot be placed safely in a header all prevent the send, and
// the diagnostic is the class alone: neither the reference nor the material nor
// the source's error text reaches a record.
func resolveCredential(ctx context.Context, src chatops.CredentialSource, req chatops.SendRequest) (authorization string, refused *chatops.Outcome) {
	d := req.Destination
	if d.Credential == "" {
		return "", nil
	}
	if src == nil {
		return "", &chatops.Outcome{Class: chatops.ClassCredentialUnavailable}
	}
	r, err := d.Recipient()
	if err != nil {
		return "", &chatops.Outcome{Class: chatops.ClassPolicyRefused}
	}
	cred, err := src.Resolve(ctx, req.Scope, d.Credential, r)
	switch {
	case errors.Is(err, chatops.ErrCredentialUnavailable):
		return "", &chatops.Outcome{Class: chatops.ClassCredentialUnavailable}
	case err != nil:
		return "", &chatops.Outcome{Class: chatops.ClassCredentialSourceDown}
	}
	if !headerSafe(cred.Bearer) {
		return "", &chatops.Outcome{Class: chatops.ClassCredentialUnavailable}
	}
	return "Bearer " + cred.Bearer, nil
}

// headerSafe accepts a non-empty visible-ASCII token without whitespace.
func headerSafe(s string) bool {
	if s == "" || len(s) > 4096 || strings.TrimSpace(s) != s {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
