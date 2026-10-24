package secretkeys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"regexp"
)

const localFormat = "aes256gcm-wrap-v1"

type keyFile struct {
	Active string            `json:"active"`
	Keys   map[string]string `json:"keys"`
}

// Local wraps with an in-process AES-256-GCM keyring.
type Local struct {
	provider string
	active   string
	keys     map[string]cipher.AEAD
}

var keyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// NewKeyfile reads a restricted keyring. production rejects group/world access.
func NewKeyfile(path string, production bool) (*Local, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, ErrUnavailable
	}
	if !stat.Mode().IsRegular() || stat.Size() > 64<<10 || stat.Mode().Perm()&0022 != 0 ||
		(production && stat.Mode().Perm()&0007 != 0) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(io.LimitReader(f, (64<<10)+1))
	d.DisallowUnknownFields()
	var cfg keyFile
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	return newLocal("keyfile", cfg)
}

// NewEphemeral generates one KEK for this process. Restart cannot unwrap.
func NewEphemeral() (*Local, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, ErrUnavailable
	}
	defer clear(key)
	return newLocal("random", keyFile{Active: "ephemeral", Keys: map[string]string{"ephemeral": base64.RawURLEncoding.EncodeToString(key)}})
}

func newLocal(provider string, cfg keyFile) (*Local, error) {
	if !keyIDPattern.MatchString(cfg.Active) || len(cfg.Keys) == 0 || len(cfg.Keys) > 64 {
		return nil, ErrInvalid
	}
	p := &Local{provider: provider, active: cfg.Active, keys: map[string]cipher.AEAD{}}
	for id, encoded := range cfg.Keys {
		if !keyIDPattern.MatchString(id) || len(encoded) != 43 {
			return nil, ErrInvalid
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if err != nil || len(raw) != 32 {
			return nil, ErrInvalid
		}
		block, err := aes.NewCipher(raw)
		clear(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		aead, err := cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			return nil, ErrInvalid
		}
		p.keys[id] = aead
	}
	if p.keys[p.active] == nil {
		return nil, ErrInvalid
	}
	return p, nil
}

func (p *Local) Ephemeral() bool { return p.provider == "random" }

func (p *Local) Wrap(ctx context.Context, key, binding []byte) (Wrapped, error) {
	if ctx.Err() != nil {
		return Wrapped{}, ErrUnavailable
	}
	if len(key) != 32 {
		return Wrapped{}, ErrInvalid
	}
	return Wrapped{Provider: p.provider, KeyRef: p.active, Format: localFormat,
		Ciphertext: p.keys[p.active].Seal(nil, nil, key, binding)}, nil
}

func (p *Local) Unwrap(ctx context.Context, w Wrapped, binding []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if w.Provider != p.provider || w.Format != localFormat || len(w.Ciphertext) != 60 || p.keys[w.KeyRef] == nil {
		return nil, ErrCiphertext
	}
	key, err := p.keys[w.KeyRef].Open(nil, nil, w.Ciphertext, binding)
	if err != nil {
		return nil, ErrCiphertext
	}
	return key, nil
}

// InitKeyfile creates an exclusive owner-only keyring. It never overwrites.
func InitKeyfile(path string) error {
	if path == "" {
		return ErrInvalid
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ErrUnavailable
	}
	defer clear(raw)
	id := newID()
	data, err := json.Marshal(keyFile{Active: id, Keys: map[string]string{id: base64.RawURLEncoding.EncodeToString(raw)}})
	if err != nil {
		return ErrInvalid
	}
	defer clear(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return ErrConflict
	}
	if err != nil {
		return ErrUnavailable
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return ErrUnavailable
	}
	if err = f.Sync(); err != nil {
		return ErrUnavailable
	}
	if err = f.Close(); err != nil {
		return ErrUnavailable
	}
	ok = true
	return nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secretkeys: entropy failed")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
