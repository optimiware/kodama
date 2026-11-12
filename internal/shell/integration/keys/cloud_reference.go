package keys

// Concrete AWS and GCP key-reference parsing.
//
// Production Preflight validates the same grammar in internal/installation; the
// two independent parsers are checked against one corpus so they cannot drift.
// ParseAWSReference and ParseGCPReference also expose the fields the concrete
// KMS clients need, and AWSReference.ValidateRegion enforces the configured
// region before a client acquires a credential or builds a request.
//
// No function here contacts a KMS, resolves an alias or rewrites a reference:
// the accepted bytes are the persisted secret key identity.

import (
	"strings"
	"unicode"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// AWSReference is a validated aws-kms: reference.
type AWSReference struct {
	// ARN reports whether the reference carried a full ARN. The alias form
	// depends on a separately configured region.
	ARN       bool
	Partition string
	Region    string
	Account   string
	KeyID     string
	Alias     string
}

// GCPReference is a validated gcp-kms: reference.
type GCPReference struct {
	Project   string
	Location  string
	KeyRing   string
	CryptoKey string
}

// ParseAWSReference validates an "aws-kms:" reference and returns its parts.
func ParseAWSReference(reference secret.KeyRef) (AWSReference, error) {
	body, ok := strings.CutPrefix(string(reference), AWSKeyRefPrefix)
	if !ok {
		return AWSReference{}, fault.New(fault.InvalidInput, "key reference is not an aws kms reference")
	}
	return parseAWSReferenceBody(body)
}

func parseAWSReferenceBody(body string) (AWSReference, error) {
	if strings.HasPrefix(body, "arn:") {
		return parseAWSReferenceARN(body)
	}
	if name, ok := strings.CutPrefix(body, "alias/"); ok {
		if !validAWSAliasName(name) {
			return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference has an invalid alias name")
		}
		return AWSReference{Alias: name}, nil
	}
	return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference must be arn:<partition>:kms:<region>:<account>:key/<key-id> or alias/<alias-name>")
}

func parseAWSReferenceARN(body string) (AWSReference, error) {
	parts := strings.SplitN(body, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference is not a well-formed ARN")
	}
	reference := AWSReference{
		ARN:       true,
		Partition: parts[1],
		Region:    parts[3],
		Account:   parts[4],
	}
	if !validAWSPartitionName(reference.Partition) {
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference names an unsupported partition")
	}
	if parts[2] != "kms" {
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference does not name the kms service")
	}
	if !validAWSRegionName(reference.Region) {
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference has an invalid region")
	}
	if !validAWSAccountID(reference.Account) {
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference has an invalid account")
	}
	switch {
	case strings.HasPrefix(parts[5], "key/"):
		reference.KeyID = strings.TrimPrefix(parts[5], "key/")
		if !validAWSKMSKeyID(reference.KeyID) {
			return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference has an invalid key id")
		}
		return reference, nil
	case strings.HasPrefix(parts[5], "alias/"):
		reference.Alias = strings.TrimPrefix(parts[5], "alias/")
		if !validAWSAliasName(reference.Alias) {
			return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference has an invalid alias name")
		}
		return reference, nil
	default:
		return AWSReference{}, fault.New(fault.InvalidInput, "aws key reference resource is neither key/<key-id> nor alias/<alias-name>")
	}
}

// ValidateRegion requires the configured AWS region to be a provider-valid
// region and, for a full ARN, to agree with the region the reference names. The
// alias form has no region of its own, so it requires a configured region.
func (reference AWSReference) ValidateRegion(configured string) error {
	if !validAWSRegionName(configured) {
		return fault.New(fault.InvalidInput, "configured aws region is invalid")
	}
	if reference.ARN && reference.Region != configured {
		return fault.New(fault.InvalidInput, "aws key reference region does not match the configured region")
	}
	return nil
}

// ParseGCPReference validates a "gcp-kms:" reference and returns its parts.
func ParseGCPReference(reference secret.KeyRef) (GCPReference, error) {
	body, ok := strings.CutPrefix(string(reference), GCPKeyRefPrefix)
	if !ok {
		return GCPReference{}, fault.New(fault.InvalidInput, "key reference is not a gcp kms reference")
	}
	parts := strings.Split(body, "/")
	if len(parts) != 8 ||
		parts[0] != "projects" || parts[2] != "locations" ||
		parts[4] != "keyRings" || parts[6] != "cryptoKeys" {
		return GCPReference{}, fault.New(fault.InvalidInput, "gcp key reference must be projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>")
	}
	if !validGCPProjectID(parts[1]) {
		return GCPReference{}, fault.New(fault.InvalidInput, "gcp key reference has an invalid project")
	}
	if !validGCPLocationName(parts[3]) {
		return GCPReference{}, fault.New(fault.InvalidInput, "gcp key reference has an invalid location")
	}
	if !validGCPResourceIdentifier(parts[5]) {
		return GCPReference{}, fault.New(fault.InvalidInput, "gcp key reference has an invalid key ring")
	}
	if !validGCPResourceIdentifier(parts[7]) {
		return GCPReference{}, fault.New(fault.InvalidInput, "gcp key reference has an invalid crypto key")
	}
	return GCPReference{
		Project:   parts[1],
		Location:  parts[3],
		KeyRing:   parts[5],
		CryptoKey: parts[7],
	}, nil
}

// validateCloudKeyReference applies the family's grammar to a reference the
// caller has already assigned to prefix. Scaleway has no structural grammar
// here: the concrete client owns the scw-kms reference shape, so only a
// non-empty name without whitespace or control characters is required.
func validateCloudKeyReference(prefix string, reference secret.KeyRef) error {
	switch prefix {
	case AWSKeyRefPrefix:
		_, err := ParseAWSReference(reference)
		return err
	case GCPKeyRefPrefix:
		_, err := ParseGCPReference(reference)
		return err
	default:
		body, ok := strings.CutPrefix(string(reference), prefix)
		if !ok || body == "" {
			return fault.New(fault.InvalidInput, "cloud key reference has no name after its family prefix")
		}
		if strings.IndexFunc(body, func(character rune) bool {
			return unicode.IsSpace(character) || unicode.IsControl(character)
		}) >= 0 || strings.TrimSpace(body) != body {
			return fault.New(fault.InvalidInput, "cloud key reference has an invalid key name after its family prefix")
		}
		return nil
	}
}

const awsMaximumAliasNameBytes = 250

// validAWSRegionName matches the same commercial, China and GovCloud region
// grammar as internal/installation.
func validAWSRegionName(region string) bool {
	if len(region) < 9 || len(region) > 32 {
		return false
	}
	parts := strings.Split(region, "-")
	if len(parts) < 3 {
		return false
	}
	for index, part := range parts[:len(parts)-1] {
		if index == 0 {
			if len(part) < 2 || len(part) > 4 {
				return false
			}
		} else if part == "" {
			return false
		}
		for _, character := range part {
			if character < 'a' || character > 'z' {
				return false
			}
		}
	}
	number := parts[len(parts)-1]
	if len(number) < 1 || len(number) > 2 {
		return false
	}
	for _, character := range number {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validAWSPartitionName(partition string) bool {
	switch partition {
	case "aws", "aws-cn", "aws-us-gov":
		return true
	default:
		return false
	}
}

func validAWSAccountID(account string) bool {
	if len(account) != 12 {
		return false
	}
	for _, character := range account {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validAWSKMSKeyID(identifier string) bool {
	if strings.HasPrefix(identifier, "mrk-") {
		return len(identifier) == len("mrk-")+32 && lowerHexDigits(identifier[len("mrk-"):])
	}
	if len(identifier) != 36 {
		return false
	}
	for index, character := range identifier {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !lowerHexDigit(character) {
				return false
			}
		}
	}
	return true
}

func lowerHexDigits(value string) bool {
	for _, character := range value {
		if !lowerHexDigit(character) {
			return false
		}
	}
	return true
}

func lowerHexDigit(character rune) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
}

func validAWSAliasName(name string) bool {
	if name == "" || len(name) > awsMaximumAliasNameBytes {
		return false
	}
	if strings.HasPrefix(name, "alias/") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '/', character == '_', character == '-':
		default:
			return false
		}
	}
	return true
}

func validGCPProjectID(project string) bool {
	if len(project) == 12 {
		digits := true
		for _, character := range project {
			if character < '0' || character > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	if len(project) < 6 || len(project) > 30 {
		return false
	}
	for index, character := range project {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
			if index == 0 {
				return false
			}
		case character == '-':
			if index == 0 || index == len(project)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validGCPLocationName(location string) bool {
	if location == "" || len(location) > 32 {
		return false
	}
	first := location[0]
	if first < 'a' || first > 'z' {
		return false
	}
	for _, character := range location {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-':
			if strings.HasPrefix(location, "-") || strings.HasSuffix(location, "-") || strings.Contains(location, "--") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validGCPResourceIdentifier(identifier string) bool {
	if len(identifier) == 0 || len(identifier) > 63 {
		return false
	}
	for index, character := range identifier {
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
			if index == 0 {
				return false
			}
		case character == '_' || character == '-':
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
