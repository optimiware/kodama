package resource

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
)

// ValidateTrustedOrigins checks every registered endpoint origin. A trusted
// origin is an absolute https URL with an authority and an optional path. The
// path may not contain empty, dot or dot-dot segments, and the origin may not
// carry userinfo, a query, a fragment, a percent-encoded byte or whitespace, so
// a registered origin cannot be reinterpreted as a different target.
//
// Nil and empty origin lists are accepted: a binding may declare no trusted
// endpoint origin.
func ValidateTrustedOrigins(origins []string) error {
	for _, origin := range origins {
		if err := validateTrustedOrigin(origin); err != nil {
			return err
		}
	}
	return nil
}

const trustedOriginScheme = "https://"

func validateTrustedOrigin(origin string) error {
	if !strings.HasPrefix(origin, trustedOriginScheme) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q is not an absolute https URL", origin))
	}
	rest := origin[len(trustedOriginScheme):]
	if rest == "" {
		return fault.New(fault.InvalidInput, "trusted origin has no authority")
	}
	if strings.ContainsAny(rest, " \t\r\n\\@?#%") {
		return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q carries a forbidden character", origin))
	}
	authority := rest
	path := ""
	if index := strings.IndexByte(rest, '/'); index >= 0 {
		authority = rest[:index]
		path = rest[index:]
	}
	if authority == "" {
		return fault.New(fault.InvalidInput, "trusted origin has no authority")
	}
	host := authority
	if index := strings.LastIndexByte(authority, ':'); index >= 0 {
		host = authority[:index]
		port := authority[index+1:]
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q has an invalid port", origin))
		}
	}
	if host == "" {
		return fault.New(fault.InvalidInput, "trusted origin has no host")
	}
	for index := 0; index < len(host); index++ {
		character := host[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '-':
		default:
			return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q has an invalid host", origin))
		}
	}
	if path == "" || path == "/" {
		return nil
	}
	path = strings.TrimSuffix(path, "/")
	for segment := range strings.SplitSeq(path[1:], "/") {
		if segment == "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q has an empty path segment", origin))
		}
		if segment == "." || segment == ".." {
			return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q has a dot path segment", origin))
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_', character == '.', character == '~':
			default:
				return fault.New(fault.InvalidInput, fmt.Sprintf("trusted origin %q has an invalid path segment", origin))
			}
		}
	}
	return nil
}

// validateReportableEvents checks the service-declared reportable event types a
// binding may accept. A declared type is a bounded report-type string and is
// never a declared lifecycle event type: a verified report keeps
// resource.verified as its lifecycle type and records the declared type in
// ReportType, so a binding cannot claim a lifecycle event or a custom
// observation as its own.
func validateReportableEvents(declared []string) error {
	for _, value := range declared {
		if event.Type(value).Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("binding reportable event %q is a lifecycle event type", value))
		}
		if !event.ReportType(value).Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("binding reportable event %q is not a valid report type", value))
		}
	}
	return nil
}
