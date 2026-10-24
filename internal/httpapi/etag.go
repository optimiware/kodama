package httpapi

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/optimiware/kodama/internal/domain"
)

// FormatETag renders a resource version as the quoted integer ETag, for
// example `"7"`.
func FormatETag(v domain.Version) (string, error) {
	if err := v.Validate(); err != nil {
		return "", err
	}
	return `"` + strconv.FormatInt(int64(v), 10) + `"`, nil
}

// SetETag sets the ETag header from the version.
func SetETag(w http.ResponseWriter, v domain.Version) error {
	tag, err := FormatETag(v)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", tag)
	return nil
}

// ParseETag parses exactly one strong quoted-integer tag. Weak tags, the
// wildcard, lists, leading zeros, signs, whitespace and out-of-range values are
// rejected: the contract equates an ETag with the integer resource version, so
// anything else is a client error rather than something to interpret.
func ParseETag(s string) (domain.Version, error) {
	if len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' {
		return 0, fmt.Errorf("%w: not a quoted strong tag", domain.ErrInvalidValue)
	}
	digits := s[1 : len(s)-1]
	if digits[0] == '0' || len(digits) > 16 {
		return 0, fmt.Errorf("%w: not a canonical version", domain.ErrInvalidValue)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, fmt.Errorf("%w: not a canonical version", domain.ErrInvalidValue)
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: not a canonical version", domain.ErrInvalidValue)
	}
	v := domain.Version(n)
	if err := v.Validate(); err != nil {
		return 0, err
	}
	return v, nil
}

// RequireIfMatch returns the expected version from the request's If-Match
// header. A missing header is 428 version_required; a malformed, weak,
// wildcard or repeated header is 400 invalid_request. The caller passes the
// version to the use case, which must compare it atomically with the write.
func RequireIfMatch(r *http.Request) (domain.Version, error) {
	values := r.Header.Values("If-Match")
	switch len(values) {
	case 0:
		return 0, VersionRequiredError()
	case 1:
	default:
		return 0, BadRequestError("If-Match must contain exactly one version")
	}
	if values[0] == "" {
		return 0, VersionRequiredError()
	}
	v, err := ParseETag(values[0])
	if err != nil {
		return 0, BadRequestError(`If-Match must be one quoted integer version such as "7"`)
	}
	return v, nil
}

// CheckVersion compares an expected version with the persisted one. It is for
// use inside the transaction that holds the row or performs the conditional
// write, never as a read followed by an unconditional write: the stale case is
// 412 version_conflict. Prefer a single conditional UPDATE ... WHERE version = $n
// and call this only to classify the zero-rows outcome.
func CheckVersion(expected, current domain.Version) error {
	if expected != current {
		return VersionConflictError()
	}
	return nil
}
