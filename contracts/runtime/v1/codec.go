package runtimev1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/optimiware/kodama/internal/domain"
)

// DecodeRequest strictly decodes and validates a request body. Besides the
// parser's rules (see Canonicalize) it rejects unknown, misspelled,
// differently-cased and omitted fields, so a nullable field must be present
// as an explicit null. Failures are *ContractError with an invalid_request
// code, except a result hash mismatch.
func DecodeRequest[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return decode[T, P](data, true)
}

// DecodeResponse decodes and validates a response body for a client. Unknown
// additional fields are tolerated (compatible additions); required fields must
// still be present.
func DecodeResponse[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return decode[T, P](data, false)
}

func decode[T any, P interface {
	*T
	Validate() error
}](data []byte, strict bool) (T, error) {
	var v T
	bad := func(format string, args ...any) (T, error) {
		var zero T
		return zero, &ContractError{Code: domain.CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
	}
	in, err := parseStrict(data)
	if err != nil {
		return bad("%v", err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return bad("%v", err)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return bad("%v", err)
	}
	out, err := parseStrict(encoded)
	if err != nil {
		return bad("%v", err)
	}
	if path, why := diffTree("", in, out, strict); why != "" {
		return bad("%s: %s", path, why)
	}
	if err := P(&v).Validate(); err != nil {
		if ce, ok := err.(*ContractError); ok {
			var zero T
			return zero, ce
		}
		return bad("%v", err)
	}
	return v, nil
}

// diffTree compares the payload tree with the tree produced by re-marshaling
// the decoded DTO. A field the DTO would write but the payload lacks is
// missing; a payload field the DTO dropped is unknown (rejected only if strict).
func diffTree(path string, in, out any, strict bool) (string, string) {
	switch o := out.(type) {
	case map[string]any:
		i, ok := in.(map[string]any)
		if !ok {
			return path, "wrong type"
		}
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			iv, present := i[k]
			if !present {
				return path + "." + k, "missing field (nullable fields must be explicit null)"
			}
			if p, why := diffTree(path+"."+k, iv, o[k], strict); why != "" {
				return p, why
			}
		}
		if strict {
			extra := make([]string, 0)
			for k := range i {
				if _, ok := o[k]; !ok {
					extra = append(extra, k)
				}
			}
			sort.Strings(extra)
			if len(extra) > 0 {
				return path + "." + extra[0], "unknown field"
			}
		}
	case []any:
		i, ok := in.([]any)
		if !ok || len(i) != len(o) {
			return path, "wrong type or length"
		}
		for n := range o {
			if p, why := diffTree(fmt.Sprintf("%s[%d]", path, n), i[n], o[n], strict); why != "" {
				return p, why
			}
		}
	default:
		if in != out {
			return path, "wrong type or value"
		}
	}
	return "", ""
}

// CanonicalResult returns the canonical bytes covered by the result hash. The
// result is validated first; the hash covers the normalized result only, never
// the submitted hash or transport metadata.
func CanonicalResult(r Result) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// ResultHash is "sha256:" and the lowercase hex SHA-256 of CanonicalResult.
func ResultHash(r Result) (Digest, error) {
	canonical, err := CanonicalResult(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}
