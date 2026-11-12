package kube

import (
	"bytes"
	"encoding/base64"
	"maps"
)

// encodeConfigMapKey encodes one absolute activation path as a Kubernetes-safe
// ConfigMap key. Kubernetes keys may only contain [-._a-zA-Z0-9], so the path
// travels as unpadded base64url.
func encodeConfigMapKey(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// decodeConfigMapKey reverses encodeConfigMapKey. A key that was not produced by
// this driver (and so does not decode to an absolute path) is preserved
// verbatim.
func decodeConfigMapKey(key string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(decoded) == 0 || decoded[0] != '/' {
		return key
	}
	return string(decoded)
}

// encodeBase64Data encodes binary values the way ConfigMap binaryData and
// Secret data require.
func encodeBase64Data(values map[string][]byte) map[string]string {
	if len(values) == 0 {
		return nil
	}
	encoded := make(map[string]string, len(values))
	for key, value := range values {
		encoded[key] = base64.StdEncoding.EncodeToString(value)
	}
	return encoded
}

// decodeBase64Data decodes a base64 value map. A malformed value is an error so
// a truncated snapshot is never mistaken for valid activation bytes.
func decodeBase64Data(values map[string]string) (map[string][]byte, error) {
	if values == nil {
		return nil, nil
	}
	decoded := make(map[string][]byte, len(values))
	for key, value := range values {
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, err
		}
		decoded[key] = data
	}
	return decoded, nil
}

// decodeConfigMapData decodes binaryData and restores the original absolute
// paths from the encoded keys.
func decodeConfigMapData(values map[string]string) (map[string][]byte, error) {
	if values == nil {
		return nil, nil
	}
	decoded := make(map[string][]byte, len(values))
	for key, value := range values {
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, err
		}
		decoded[decodeConfigMapKey(key)] = data
	}
	return decoded, nil
}

// sameBytes reports whether two binary maps carry exactly the same content.
func sameBytes(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || !bytes.Equal(value, other) {
			return false
		}
	}
	return true
}

// copyStringMap copies one label or annotation map so a response can never
// mutate the caller's value.
func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}
