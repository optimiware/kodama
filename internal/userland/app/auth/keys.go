package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/optimiware/kodama/internal/fault"
)

const (
	signingKeyFilename   = "signing-key.pem"
	cookieSecretFilename = "cookie-secret.key"
	githubOAuthFilename  = "github-oauth.json"
	cookieSecretBytes    = 32
)

type jwkPublic struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwksResponse struct {
	Keys []jwkPublic `json:"keys"`
}

type signingMaterial struct {
	privateKey   *ecdsa.PrivateKey
	kid          string
	jwk          jwkPublic
	jwksJSON     []byte
	cookieSecret []byte
}

type persistedGitHubCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func loadOrCreateMaterial(stateDir string, random io.Reader) (*signingMaterial, error) {
	var priv *ecdsa.PrivateKey
	var cookieSecret []byte
	var err error

	if stateDir == "" {
		priv, err = ecdsa.GenerateKey(elliptic.P256(), random)
		if err != nil {
			return nil, fault.Wrap(fault.InternalFailure, "generate ES256 signing key", err)
		}
		cookieSecret = make([]byte, cookieSecretBytes)
		if _, err := io.ReadFull(random, cookieSecret); err != nil {
			return nil, fault.Wrap(fault.InternalFailure, "generate cookie secret", err)
		}
	} else {
		cleanDir := filepath.Clean(stateDir)
		if err := os.MkdirAll(cleanDir, 0o700); err != nil {
			return nil, fault.Wrap(fault.DependencyUnavailable, "create auth state directory", err)
		}
		keyPath := filepath.Join(cleanDir, signingKeyFilename)
		priv, err = loadOrCreateSigningKey(keyPath, random)
		if err != nil {
			return nil, err
		}
		secretPath := filepath.Join(cleanDir, cookieSecretFilename)
		cookieSecret, err = loadOrCreateCookieSecret(secretPath, random)
		if err != nil {
			return nil, err
		}
	}

	return buildSigningMaterial(priv, cookieSecret)
}

func buildSigningMaterial(priv *ecdsa.PrivateKey, cookieSecret []byte) (*signingMaterial, error) {
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, fault.New(fault.InvalidInput, "signing key must be ECDSA P-256")
	}
	xBytes := priv.PublicKey.X.FillBytes(make([]byte, 32))
	yBytes := priv.PublicKey.Y.FillBytes(make([]byte, 32))
	xB64 := base64.RawURLEncoding.EncodeToString(xBytes)
	yB64 := base64.RawURLEncoding.EncodeToString(yBytes)

	// Deterministic RFC 7638 JWK thumbprint over the canonical P-256 public key.
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, xB64, yB64)
	thumb := sha256.Sum256([]byte(canonical))
	kid := base64.RawURLEncoding.EncodeToString(thumb[:16])

	pub := jwkPublic{
		Kty: "EC",
		Crv: "P-256",
		Alg: "ES256",
		Use: "sig",
		Kid: kid,
		X:   xB64,
		Y:   yB64,
	}
	encodedJWKS, err := json.Marshal(jwksResponse{Keys: []jwkPublic{pub}})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "marshal JWKS", err)
	}
	return &signingMaterial{
		privateKey:   priv,
		kid:          kid,
		jwk:          pub,
		jwksJSON:     encodedJWKS,
		cookieSecret: append([]byte(nil), cookieSecret...),
	}, nil
}

func loadOrCreateSigningKey(path string, random io.Reader) (*ecdsa.PrivateKey, error) {
	if data, err := readPrivateFile0600(path); err == nil {
		return parseSigningKeyPEM(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	generated, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "generate ES256 signing key", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(generated)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "marshal ES256 signing key", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	created, err := createPrivateFileOnce0600(path, pemBytes)
	if err != nil {
		return nil, err
	}
	if created {
		return generated, nil
	}
	data, err := readPrivateFile0600(path)
	if err != nil {
		return nil, err
	}
	return parseSigningKeyPEM(data)
}

func parseSigningKeyPEM(pemData []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fault.New(fault.InvalidInput, "signing key file is not valid PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ecKey, ok := parsed.(*ecdsa.PrivateKey)
		if !ok || ecKey.Curve != elliptic.P256() {
			return nil, fault.New(fault.InvalidInput, "signing key file does not hold an ECDSA P-256 key")
		}
		return ecKey, nil
	}
	if ecKey, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		if ecKey.Curve != elliptic.P256() {
			return nil, fault.New(fault.InvalidInput, "signing key file does not hold an ECDSA P-256 key")
		}
		return ecKey, nil
	}
	return nil, fault.New(fault.InvalidInput, "signing key file could not be parsed")
}

func loadOrCreateCookieSecret(path string, random io.Reader) ([]byte, error) {
	if data, err := readPrivateFile0600(path); err == nil {
		if len(data) != cookieSecretBytes {
			return nil, fault.New(fault.InvalidInput, "cookie secret file has an unexpected length")
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	secret := make([]byte, cookieSecretBytes)
	if _, err := io.ReadFull(random, secret); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "generate cookie secret", err)
	}
	created, err := createPrivateFileOnce0600(path, secret)
	if err != nil {
		return nil, err
	}
	if created {
		return secret, nil
	}
	data, err := readPrivateFile0600(path)
	if err != nil {
		return nil, err
	}
	if len(data) != cookieSecretBytes {
		return nil, fault.New(fault.InvalidInput, "cookie secret file has an unexpected length")
	}
	return data, nil
}

func loadPersistedGitHubCredentials(stateDir string) (persistedGitHubCredentials, bool, error) {
	if stateDir == "" {
		return persistedGitHubCredentials{}, false, nil
	}
	path := filepath.Join(filepath.Clean(stateDir), githubOAuthFilename)
	data, err := readPrivateFile0600(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return persistedGitHubCredentials{}, false, nil
		}
		return persistedGitHubCredentials{}, false, err
	}
	var creds persistedGitHubCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return persistedGitHubCredentials{}, false, fault.New(fault.InvalidInput, "persisted GitHub OAuth credentials are malformed")
	}
	if creds.ClientID == "" || creds.ClientSecret == "" {
		return persistedGitHubCredentials{}, false, fault.New(fault.InvalidInput, "persisted GitHub OAuth credentials are incomplete")
	}
	return creds, true, nil
}

func savePersistedGitHubCredentials(stateDir string, creds persistedGitHubCredentials) error {
	if stateDir == "" {
		return nil
	}
	cleanDir := filepath.Clean(stateDir)
	if err := os.MkdirAll(cleanDir, 0o700); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "create auth state directory", err)
	}
	payload, err := json.Marshal(creds)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "marshal GitHub OAuth credentials", err)
	}
	return replacePrivateFile0600(filepath.Join(cleanDir, githubOAuthFilename), payload)
}

func readPrivateFile0600(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fault.Wrap(fault.DependencyUnavailable, "inspect private state file", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput, "private state file must be a regular file, not a symlink or directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fault.New(fault.InvalidInput, "private state file permissions are too open; expected 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "read private state file", err)
	}
	return data, nil
}

// createPrivateFileOnce0600 writes content to a 0600 sibling temporary file and
// links it into place so concurrent initializers never clobber an existing file.
func createPrivateFileOnce0600(path string, content []byte) (bool, error) {
	parent := filepath.Dir(path)
	temp, err := os.CreateTemp(parent, ".kodama-auth-*")
	if err != nil {
		return false, fault.Wrap(fault.DependencyUnavailable, "create temporary state file", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return false, fault.Wrap(fault.DependencyUnavailable, "restrict temporary state file", err)
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return false, fault.Wrap(fault.DependencyUnavailable, "write temporary state file", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return false, fault.Wrap(fault.DependencyUnavailable, "sync temporary state file", err)
	}
	if err := temp.Close(); err != nil {
		return false, fault.Wrap(fault.DependencyUnavailable, "close temporary state file", err)
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fault.Wrap(fault.DependencyUnavailable, "publish private state file", err)
	}
	return true, nil
}

func replacePrivateFile0600(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fault.New(fault.InvalidInput, "destination state file is not a regular file")
	}
	parent := filepath.Dir(path)
	temp, err := os.CreateTemp(parent, ".kodama-auth-*")
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "create temporary state file", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "restrict temporary state file", err)
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "write temporary state file", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "sync temporary state file", err)
	}
	if err := temp.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "close temporary state file", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "replace private state file", err)
	}
	return nil
}
