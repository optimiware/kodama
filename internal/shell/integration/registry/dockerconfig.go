package registry

// Docker config.json credential source.
//
// The production entry points this source at one trusted file named by an
// explicit environment setting (KODAMA_REGISTRY_CONFIG_FILE). The document is
// read once at startup: a missing, oversized or malformed file fails closed
// before any resolution, and later edits are never observed. Entries are
// ordinary Docker auths keys (a registry host, optionally with a scheme and a
// path); Docker Hub's conventional index.docker.io/v1/ key is mapped to the API
// host the resolver actually contacts. The source answers only the origin it is
// asked about and never renders a credential into an error.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// maxDockerConfigBytes bounds one credential file. A docker config.json is a
// few kilobytes; the bound keeps a mistaken path to a huge file from being read
// into memory.
const maxDockerConfigBytes int64 = 1 << 20

// DockerConfig is the immutable installation credential source backed by a
// Docker config.json file. It is safe for concurrent use.
type DockerConfig struct {
	credentials map[string]Credential
}

var _ CredentialSource = (*DockerConfig)(nil)

// dockerConfigAuth is one Docker auths entry. identitytoken is the Docker
// config.json spelling.
type dockerConfigAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// LoadDockerConfig reads one docker config.json credential file from a trusted
// path. A read failure, an oversized file and a malformed document are all
// configuration refusals; the path and its content are never rendered.
func LoadDockerConfig(path string) (*DockerConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fault.New(fault.InvalidInput, "registry credential file path is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "registry credential file is unreadable")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxDockerConfigBytes+1))
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "registry credential file is unreadable")
	}
	if int64(len(data)) > maxDockerConfigBytes {
		return nil, fault.New(fault.InvalidInput, "registry credential file exceeds the size bound")
	}
	return ParseDockerConfig(data)
}

// ParseDockerConfig parses one docker config.json document.
func ParseDockerConfig(data []byte) (*DockerConfig, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fault.New(fault.InvalidInput, "registry credential file is empty")
	}
	var document struct {
		Auths *map[string]dockerConfigAuth `json:"auths"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return nil, fault.New(fault.InvalidInput, "registry credential file is not valid JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fault.New(fault.InvalidInput, "registry credential file is not valid JSON")
	}
	if document.Auths == nil {
		return nil, fault.New(fault.InvalidInput, "registry credential file has no auths object")
	}
	credentials := make(map[string]Credential, len(*document.Auths))
	for registryKey, entry := range *document.Auths {
		host, ok := dockerConfigHost(registryKey)
		if !ok {
			return nil, fault.New(fault.InvalidInput, "registry credential entry names an invalid registry")
		}
		credential, err := entry.credential()
		if err != nil {
			return nil, err
		}
		if existing, duplicate := credentials[host]; duplicate && existing != credential {
			return nil, fault.New(fault.InvalidInput, "registry credential file configures one registry twice")
		}
		credentials[host] = credential
	}
	return &DockerConfig{credentials: credentials}, nil
}

// Credential returns the configured credential for one origin. An origin with
// no entry (or an origin that cannot name a registry) is anonymous; the source
// never guesses that another origin's credential applies.
func (source *DockerConfig) Credential(ctx context.Context, origin string) (Credential, error) {
	if source == nil {
		return Credential{Scheme: CredentialAnonymous}, nil
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return Credential{Scheme: CredentialAnonymous}, nil
	}
	host, ok := dockerConfigHost(parsed.Scheme + "://" + parsed.Host)
	if !ok {
		return Credential{Scheme: CredentialAnonymous}, nil
	}
	credential, ok := source.credentials[host]
	if !ok {
		return Credential{Scheme: CredentialAnonymous}, nil
	}
	return credential, nil
}

// credential renders one auths entry. auth is the conventional base64
// username:password pair; username/password are accepted as separate fields,
// and identitytoken is a Bearer token. An entry with none of these is refused
// rather than treated as anonymous.
func (entry dockerConfigAuth) credential() (Credential, error) {
	if entry.Auth != "" {
		decoded, ok := decodeDockerAuth(entry.Auth)
		if !ok {
			return Credential{}, fault.New(fault.InvalidInput, "registry credential entry has an invalid auth value")
		}
		username, password, found := strings.Cut(decoded, ":")
		if !found || username == "" {
			return Credential{}, fault.New(fault.InvalidInput, "registry credential entry has an invalid auth value")
		}
		return Credential{Scheme: CredentialBasic, Username: username, Password: password}, nil
	}
	if entry.Username != "" || entry.Password != "" {
		if entry.Username == "" {
			return Credential{}, fault.New(fault.InvalidInput, "registry credential entry has a password without a username")
		}
		return Credential{Scheme: CredentialBasic, Username: entry.Username, Password: entry.Password}, nil
	}
	if entry.IdentityToken != "" {
		return Credential{Scheme: CredentialBearer, Token: entry.IdentityToken}, nil
	}
	return Credential{}, fault.New(fault.InvalidInput, "registry credential entry has no supported credential")
}

// decodeDockerAuth decodes Docker's base64 username:password value. Standard
// padded and unpadded encodings are both accepted.
func decodeDockerAuth(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return string(decoded), true
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return string(decoded), true
	}
	return "", false
}

// dockerConfigHost normalizes one auths key to the registry host the resolver
// contacts. A key may carry a scheme and a path (Docker Hub's conventional
// https://index.docker.io/v1/ form); the API host for every Docker Hub alias is
// registry-1.docker.io, matching apiHost. The scheme is not part of the match:
// an installation-level credential belongs to the registry host.
func dockerConfigHost(key string) (string, bool) {
	value := strings.TrimSpace(key)
	if value == "" {
		return "", false
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "", false
		}
		value = parsed.Host
	} else if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	value = strings.ToLower(strings.TrimSuffix(value, "/"))
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", false
	}
	switch value {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "auth.docker.io":
		// Docker Hub names the image under docker.io/index.docker.io and serves
		// the API at registry-1.docker.io; its Bearer token realm is on
		// auth.docker.io, the one token origin the conventional Docker config
		// key must cover for a private image pull.
		return "registry-1.docker.io", true
	default:
		return value, true
	}
}
