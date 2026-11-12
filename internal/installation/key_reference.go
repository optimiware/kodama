package installation

// Provider key-reference grammars.
//
// The installation layer may not import Shell, so it validates the AWS and GCP
// reference shapes locally. The keys package keeps its own copy for provider
// construction and the later concrete clients; both are checked against the one
// corpus in test/contracts/adapters/testdata/kms_references.json, so the
// two parsers cannot drift into accepting different references.
//
// These functions are pure text rules. They never contact a KMS, resolve an
// alias, normalize a reference or acquire a credential. The accepted reference
// bytes are the persisted identity.

import (
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// AWS reference bounds and character sets. A KMS alias name is at most 256
// characters including its "alias/" resource prefix, so the name after that
// prefix is bounded at 250.
const awsMaxAliasNameBytes = 250

// validAWSRegion matches a commercial, China or GovCloud region name. Regions
// are lower-case and end in a numbered location.
func validAWSRegion(region string) bool {
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

// validAWSKeyID accepts exactly the identifiers AWS KMS returns: a UUID or a
// multi-region "mrk-" identifier. A bare alias, bare account or arbitrary text
// is not a key id.
func validAWSKeyID(identifier string) bool {
	if strings.HasPrefix(identifier, "mrk-") {
		return len(identifier) == len("mrk-")+32 && lowerHex(identifier[len("mrk-"):])
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
			if !lowerHexRune(character) {
				return false
			}
		}
	}
	return true
}

func lowerHex(value string) bool {
	for _, character := range value {
		if !lowerHexRune(character) {
			return false
		}
	}
	return true
}

func lowerHexRune(character rune) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
}

// validAWSAlias accepts the AWS alias-name character set after its "alias/"
// resource prefix. An empty segment, a repeated resource prefix, a URL escape
// and a traversal segment are not provider-valid names.
func validAWSAlias(name string) bool {
	if name == "" || len(name) > awsMaxAliasNameBytes {
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

func validAWSPartition(partition string) bool {
	switch partition {
	case "aws", "aws-cn", "aws-us-gov":
		return true
	default:
		return false
	}
}

func validAWSAccount(account string) bool {
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

// validateAWSKeyReference validates the reference body after the "aws-kms:"
// family prefix.
func validateAWSKeyReference(body string) error {
	if strings.HasPrefix(body, "arn:") {
		return validateAWSKeyARN(body)
	}
	if name, ok := strings.CutPrefix(body, "alias/"); ok {
		if !validAWSAlias(name) {
			return fault.New(fault.InvalidInput, "aws key reference has an invalid alias name")
		}
		return nil
	}
	return fault.New(fault.InvalidInput, "aws key reference must be arn:<partition>:kms:<region>:<account>:key/<key-id> or alias/<alias-name>")
}

func validateAWSKeyARN(body string) error {
	parts := strings.SplitN(body, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return fault.New(fault.InvalidInput, "aws key reference is not a well-formed ARN")
	}
	partition, service, region, account, resource := parts[1], parts[2], parts[3], parts[4], parts[5]
	if !validAWSPartition(partition) {
		return fault.New(fault.InvalidInput, "aws key reference names an unsupported partition")
	}
	if service != "kms" {
		return fault.New(fault.InvalidInput, "aws key reference does not name the kms service")
	}
	if !validAWSRegion(region) {
		return fault.New(fault.InvalidInput, "aws key reference has an invalid region")
	}
	if !validAWSAccount(account) {
		return fault.New(fault.InvalidInput, "aws key reference has an invalid account")
	}
	switch {
	case strings.HasPrefix(resource, "key/"):
		if !validAWSKeyID(strings.TrimPrefix(resource, "key/")) {
			return fault.New(fault.InvalidInput, "aws key reference has an invalid key id")
		}
		return nil
	case strings.HasPrefix(resource, "alias/"):
		if !validAWSAlias(strings.TrimPrefix(resource, "alias/")) {
			return fault.New(fault.InvalidInput, "aws key reference has an invalid alias name")
		}
		return nil
	default:
		return fault.New(fault.InvalidInput, "aws key reference resource is neither key/<key-id> nor alias/<alias-name>")
	}
}

// validGCPProject accepts a GCP project id or its 12-digit project number.
func validGCPProject(project string) bool {
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

// validGCPLocation accepts "global", a multi-region location such as "us", or
// a region such as "europe-west1". Upper-case letters, dots and empty segments
// are not provider-valid.
func validGCPLocation(location string) bool {
	if location == "" || len(location) > 32 {
		return false
	}
	if location[0] < 'a' || location[0] > 'z' {
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

// validGCPResourceID accepts a key-ring or crypto-key id: 1 to 63 characters
// that begin with a letter and contain only letters, digits, hyphens and
// underscores.
func validGCPResourceID(identifier string) bool {
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

// validateGCPKeyReference validates the reference body after the "gcp-kms:"
// family prefix.
func validateGCPKeyReference(body string) error {
	parts := strings.Split(body, "/")
	if len(parts) != 8 ||
		parts[0] != "projects" || parts[2] != "locations" ||
		parts[4] != "keyRings" || parts[6] != "cryptoKeys" {
		return fault.New(fault.InvalidInput, "gcp key reference must be projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>")
	}
	if !validGCPProject(parts[1]) {
		return fault.New(fault.InvalidInput, "gcp key reference has an invalid project")
	}
	if !validGCPLocation(parts[3]) {
		return fault.New(fault.InvalidInput, "gcp key reference has an invalid location")
	}
	if !validGCPResourceID(parts[5]) {
		return fault.New(fault.InvalidInput, "gcp key reference has an invalid key ring")
	}
	if !validGCPResourceID(parts[7]) {
		return fault.New(fault.InvalidInput, "gcp key reference has an invalid crypto key")
	}
	return nil
}
