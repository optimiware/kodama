package production

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/pkg/secretkeys"
)

// Provider names accepted by Config.Provider. Anything else, including the
// ephemeral and keyfile providers of pkg/secretkeys, is rejected.
const (
	AWS      = secretkeys.ProviderAWS
	GCP      = secretkeys.ProviderGCP
	Scaleway = secretkeys.ProviderScaleway
)

const (
	// DefaultProbeTimeout bounds the startup round trip when Config.ProbeTimeout is zero.
	DefaultProbeTimeout = 10 * time.Second
	// MaxProbeTimeout is the largest accepted startup probe bound.
	MaxProbeTimeout = 2 * time.Minute

	maxVersions = 64
)

// Config selects one cloud KMS and names the KEK versions it may use. It holds
// no credential. It is copied on construction; later changes have no effect.
type Config struct {
	// Provider is "aws", "gcp" or "scaleway". It is never inferred from
	// credentials, endpoints or residency.
	Provider string
	// KEKID is the logical key identity recorded in every KEK reference and
	// authenticated in every binding. It stays the same across rotation.
	KEKID string
	// Current is the immutable version label used for every new wrap.
	Current string
	// Resources maps immutable version labels to cloud key resources: the
	// current key and every retained key that may still unwrap stored scope
	// keys or backups. A label must never be repointed at another key, and a
	// retained entry must not be removed while a wrap or backup refers to it.
	//
	//   aws:      key UUID, "mrk-" multi-region key id, or key ARN (never an alias)
	//   gcp:      projects/P/locations/L/keyRings/R/cryptoKeys/K (never a version)
	//   scaleway: key UUID, or "fr-par/<uuid>"
	Resources map[string]string
	// ProbeTimeout bounds the startup round trip; zero selects DefaultProbeTimeout.
	ProbeTimeout time.Duration
}

// resource is one validated, normalized KEK version.
type resource struct {
	label string
	// id is what the pkg/secretkeys constructor takes (bare UUID for Scaleway).
	id string
	// keyRef is what that provider records in Wrapped.KeyRef.
	keyRef string
}

// plan is the validated, immutable form of Config.
type plan struct {
	provider string
	format   string
	kekID    string
	current  resource
	retained []resource
	byLabel  map[string]resource
	timeout  time.Duration
}

var (
	awsKeyID  = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)
	awsARN    = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)
	gcpKey    = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}/locations/[a-z0-9-]{1,63}/keyRings/[A-Za-z0-9_-]{1,63}/cryptoKeys/[A-Za-z0-9_-]{1,63}$`)
	scalewayI = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

func formatFor(provider string) string {
	switch provider {
	case AWS:
		return secretkeys.FormatAWS
	case GCP:
		return secretkeys.FormatGCP
	default:
		return secretkeys.FormatScaleway
	}
}

// normalize validates one cloud resource. Mutable or ambiguous references are
// rejected so a stored wrap always resolves to the same key.
func normalize(provider, label, raw string) (resource, error) {
	switch provider {
	case AWS:
		if strings.Contains(raw, "alias/") {
			return resource{}, invalid("version %q: AWS key aliases are mutable; use a key ID or ARN", label)
		}
		if !awsKeyID.MatchString(raw) && !awsARN.MatchString(raw) {
			return resource{}, invalid("version %q: not an AWS KMS key ID or key ARN", label)
		}
		return resource{label: label, id: raw, keyRef: raw}, nil
	case GCP:
		if !gcpKey.MatchString(raw) {
			return resource{}, invalid("version %q: not a Cloud KMS CryptoKey resource name (a key version path is not accepted)", label)
		}
		return resource{label: label, id: raw, keyRef: raw}, nil
	default:
		id, _ := strings.CutPrefix(raw, secretkeys.ScalewayRefPrefix)
		if !scalewayI.MatchString(id) {
			return resource{}, invalid("version %q: Scaleway keys are UUIDs, optionally prefixed %q (only that region is supported)", label, secretkeys.ScalewayRefPrefix)
		}
		return resource{label: label, id: id, keyRef: secretkeys.ScalewayRefPrefix + id}, nil
	}
}

// validate checks the whole configuration without contacting any provider.
func (c Config) Validate() error {
	_, err := c.validate()
	return err
}

func (c Config) validate() (plan, error) {
	switch c.Provider {
	case "":
		return plan{}, ErrProviderRequired
	case AWS, GCP, Scaleway:
	default:
		return plan{}, fmt.Errorf("%w: %q (use aws, gcp or scaleway)", ErrUnsupportedProvider, sanitize(c.Provider))
	}
	if !validField(c.KEKID) {
		return plan{}, invalid("KEK identity must be 1..%d printable bytes", maxField)
	}
	if !validField(c.Current) {
		return plan{}, invalid("the current version label must be 1..%d printable bytes", maxField)
	}
	if len(c.Resources) == 0 || len(c.Resources) > maxVersions {
		return plan{}, invalid("1..%d version resources are required", maxVersions)
	}
	if c.ProbeTimeout < 0 || c.ProbeTimeout > MaxProbeTimeout {
		return plan{}, invalid("probe timeout must be within 0..%s", MaxProbeTimeout)
	}
	p := plan{provider: c.Provider, format: formatFor(c.Provider), kekID: c.KEKID, timeout: c.ProbeTimeout, byLabel: make(map[string]resource, len(c.Resources))}
	if p.timeout == 0 {
		p.timeout = DefaultProbeTimeout
	}
	labels := make([]string, 0, len(c.Resources))
	for label := range c.Resources {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	seen := map[string]string{}
	for _, label := range labels {
		if !validField(label) {
			return plan{}, invalid("version labels must be 1..%d printable bytes", maxField)
		}
		r, err := normalize(c.Provider, label, c.Resources[label])
		if err != nil {
			return plan{}, err
		}
		// Scaleway UUIDs are case-insensitive; AWS ids are lowercase by
		// validation and GCP resource names are case-sensitive, so they
		// compare exactly.
		key := r.keyRef
		if c.Provider == Scaleway {
			key = strings.ToLower(key)
		}
		if other, dup := seen[key]; dup {
			return plan{}, invalid("versions %q and %q name the same key resource", other, label)
		}
		seen[key] = label
		p.byLabel[label] = r
		if label != c.Current {
			p.retained = append(p.retained, r)
		}
	}
	cur, ok := p.byLabel[c.Current]
	if !ok {
		return plan{}, invalid("the current version %q has no configured resource", c.Current)
	}
	p.current = cur
	return p, nil
}

// sanitize bounds and cleans an operator-supplied value echoed in an error.
func sanitize(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
