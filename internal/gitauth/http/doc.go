// Package gitauthhttp is the HTTP adapter of the Git authority: the credential
// exchange POST /tenants/{tenantId}/git-credentials (api/git-credentials.yaml).
// It reuses the shared router, bounded body reading and error envelopes of
// internal/httpapi and contains no persistence or token code. Nothing registers
// it until the assembly task (T-132); the route's authorization rule is Rule.
package gitauthhttp
