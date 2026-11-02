package gitworker

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"regexp"
)

// tokenRe is the compact form of an issued token (a JWT or an opaque URL-safe
// string): no whitespace, no PEM armor, nothing that could be a key file.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

const maxPEMBlocks = 16

// pemOnly accepts a sequence of PEM blocks of one type and nothing else, and
// returns them re-encoded. pem.Decode skips malformed text while it searches for
// a block, so the span each block was decoded from is checked here: it must start
// with the block's own BEGIN line, hold exactly one BEGIN and one END line and end
// with the block's END line, so no byte outside a validated block (an
// unterminated private-key block included) is non-whitespace. The delivered
// material is built only from the validated blocks, never from the input bytes.
// A block of another type, one with headers, one that does not parse and any
// document that is not PEM (a JWK set, with or without private members) is refused.
func pemOnly(b []byte, typ string, parse func([]byte) error) ([]byte, error) {
	if len(b) == 0 || len(b) > 64<<10 {
		return nil, errors.New("empty or too large")
	}
	begin, end := []byte("-----BEGIN "), []byte("-----END "+typ+"-----")
	var out []byte
	rest, n := b, 0
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			if len(bytes.TrimSpace(rest)) != 0 {
				return nil, errors.New("data outside a block")
			}
			break
		}
		span := bytes.TrimSpace(rest[:len(rest)-len(next)])
		if !bytes.HasPrefix(span, []byte("-----BEGIN "+typ+"-----")) || !bytes.HasSuffix(span, end) ||
			bytes.Count(span, begin) != 1 || bytes.Count(span, []byte("-----END ")) != 1 {
			return nil, errors.New("data outside a block")
		}
		if block.Type != typ || len(block.Headers) != 0 {
			return nil, errors.New("unexpected block")
		}
		if err := parse(block.Bytes); err != nil {
			return nil, err
		}
		if n++; n > maxPEMBlocks {
			return nil, errors.New("too many blocks")
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: block.Bytes})...)
		rest = next
	}
	if n == 0 {
		return nil, errors.New("no block")
	}
	return out, nil
}

// checkPublicKeys requires PEM "PUBLIC KEY" blocks (PKIX), the form the Git
// authority's key ring distributes, and returns them re-encoded.
func checkPublicKeys(b []byte) ([]byte, error) {
	return pemOnly(b, "PUBLIC KEY", func(der []byte) error {
		_, err := x509.ParsePKIXPublicKey(der)
		return err
	})
}

// checkCA requires PEM "CERTIFICATE" blocks and returns them re-encoded.
func checkCA(b []byte) ([]byte, error) {
	return pemOnly(b, "CERTIFICATE", func(der []byte) error {
		_, err := x509.ParseCertificate(der)
		return err
	})
}
