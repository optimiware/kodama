package artifacts

import (
	"fmt"
	"regexp"
	"strings"
)

// RegistryRef is a parsed registry repository reference. The caller's spelling
// is only ever a request to resolve: ownership and provenance come from the
// injected policy and trusted context, never from this value.
type RegistryRef struct {
	Host       string // lowercase host with optional :port
	Repository string // slash-separated lowercase path
}

var (
	registryHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	repoPartRe     = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe          = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ParseRegistryRef parses "host[:port]/repository[:tag]". A tag is accepted
// and discarded: content is addressed by the separate digest, and a tag may
// move. A scheme, credentials, query, digest or dot segment is rejected before
// any network access is considered.
func ParseRegistryRef(s string) (RegistryRef, error) {
	bad := func() (RegistryRef, error) { return RegistryRef{}, invalidf("registry must be host[:port]/repository") }
	if s == "" || len(s) > 512 || strings.ContainsAny(s, "@?#\\ \t\r\n") || strings.Contains(s, "://") {
		return bad()
	}
	host, rest, ok := strings.Cut(s, "/")
	if !ok || !registryHostRe.MatchString(host) || rest == "" {
		return bad()
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		if strings.Contains(rest[i:], "/") || !tagRe.MatchString(rest[i+1:]) {
			return bad()
		}
		rest = rest[:i]
	}
	if len(rest) > 255 {
		return bad()
	}
	for _, part := range strings.Split(rest, "/") {
		if !repoPartRe.MatchString(part) {
			return bad()
		}
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		var port int
		if _, err := fmt.Sscanf(host[i+1:], "%d", &port); err != nil || port < 1 || port > 65535 {
			return bad()
		}
	}
	return RegistryRef{Host: host, Repository: rest}, nil
}

// String is the canonical form stored with the registration.
func (r RegistryRef) String() string { return r.Host + "/" + r.Repository }
