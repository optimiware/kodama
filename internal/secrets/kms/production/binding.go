package production

import (
	"encoding/binary"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// bindingDomain separates this framing from every other use of the cloud
// providers' associated data. Changing the framing changes every stored
// binding, so the golden vectors in binding_test.go freeze it.
const bindingDomain = "kodama/scope-key-kms-binding/v1"

const maxField = 256

func frame(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func validField(s string) bool {
	if s == "" || len(s) > maxField || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// binding frames the authenticated data of one scope-key wrap: the installation,
// tenant, scope-key identity and generation (the consumer's WrapContext), and
// the KEK reference plus wrap format that produced the ciphertext. Every string
// is length-delimited, so no field boundary can be shifted by the content of
// another. It validates before any provider is contacted.
func binding(wc secrets.WrapContext, ref secrets.KEKRef, format string) ([]byte, error) {
	if !validField(wc.InstallationID) || wc.TenantID.Validate() != nil || !validField(wc.KeyID) ||
		wc.Generation < 1 || wc.Generation > domain.MaxSafeInteger {
		return nil, keyError(secrets.KeyCorrupt, "bind", errWrapContext)
	}
	if !validField(ref.Provider) || !validField(ref.ID) || !validField(ref.Version) || !validField(format) {
		return nil, keyError(secrets.KeyCorrupt, "bind", errWrapContext)
	}
	b := frame(nil, bindingDomain)
	for _, f := range []string{wc.InstallationID, string(wc.TenantID), wc.KeyID, ref.Provider, format, ref.ID, ref.Version} {
		b = frame(b, f)
	}
	return binary.BigEndian.AppendUint64(b, uint64(wc.Generation)), nil
}
