package installation

// Explicit production transport rules.
//
// Production requires TLS on every connection the installation declares: the
// NATS URL must be a single tls:// URL without userinfo, a comma list or a
// fragment, and every PostgreSQL DSN must be a URL-form postgres:// DSN
// carrying exactly one sslmode=verify-full option.
//
// These are strict allowlist validators, not general URL parsers. The driver
// that consumes a PostgreSQL DSN is pgx, whose pgconn parser follows libpq and
// is not net/url: it treats a fragment as query text, accepts the ssl=true
// alias, accepts host and unix-socket query overrides and supports comma host
// lists. A validator built on net/url therefore accepts DSNs the driver would
// connect without verified TLS. This package instead accepts only a narrow
// syntax that is unambiguous for the driver:
//
//   - the scheme is the exact lower-case postgres:// (the postgresql://
//     spelling is deliberately refused);
//   - no fragment marker and no whitespace, control or non-ASCII byte
//     appears anywhere in the raw string;
//   - the authority carries optional percent-free userinfo and exactly one
//     host (no comma list and no Unix-socket path), with an optional port in
//     1-65535;
//   - the path names exactly one database;
//   - the query allowlist is sslmode, sslrootcert, connect_timeout and
//     application_name, each at most once. sslmode must be spelled exactly
//     and its percent-decoded value must be exactly verify-full. Every other
//     key (host, hostaddr, ssl, service, passfile, sslcert, sslkey,
//     target_session_attrs, options, sslnegotiation, ...) and every
//     percent-encoded key spelling is refused.
//
// The validator never imports pgx: the architecture checker forbids external
// modules in internal/installation. test/contracts/adapters contains the
// differential test that runs pgconn.ParseConfig over everything this
// validator accepts, so the allowlist cannot drift from the driver silently.
//
// The NATS validator is a strict single-URL validator: exactly one broker, no
// userinfo in any form (including the empty tls://@host form), no comma list,
// no fragment marker, and an optional port in 1-65535. nats.go splits a comma
// list and applies userinfo per element, so those forms would smuggle an
// unverified connection past a host-only check; a missing port stays allowed
// because nats.go supplies its default.
//
// Every refusal is fault.InvalidInput and names only the setting: a configured
// DSN, URL, userinfo or password is never echoed.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// postgresURLPrefix is the only accepted PostgreSQL URL scheme spelling.
const postgresURLPrefix = "postgres://"

// natsTLSURLPrefix is the only accepted production NATS URL scheme spelling.
const natsTLSURLPrefix = "tls://"

// productionPostgresQueryKeys is the complete production DSN query allowlist.
// It names only options the chart/scripts/test producers use and that cannot
// weaken TLS: sslmode carries the required mode, sslrootcert changes the trust
// roots, and connect_timeout/application_name are runtime parameters.
var productionPostgresQueryKeys = map[string]bool{
	"sslmode":          true,
	"sslrootcert":      true,
	"connect_timeout":  true,
	"application_name": true,
}

// ValidateProductionPostgresURL refuses a PostgreSQL DSN that is not a
// URL-form postgres:// DSN with exactly one sslmode=verify-full option. It is
// the rule Preflight applies to the runtime DSN and the composition applies to
// the controller, publisher and auth DSNs.
func ValidateProductionPostgresURL(value string) error {
	return validateProductionPostgresURL(value, "production database URL")
}

func validateProductionPostgresURL(value, setting string) error {
	if strings.ContainsRune(value, '#') {
		return invalidTransport(setting, "must not carry a fragment")
	}
	if err := checkTransportBytes(value, setting); err != nil {
		return err
	}
	if !strings.HasPrefix(value, postgresURLPrefix) {
		// This also refuses the postgresql:// spelling and every upper-case
		// scheme byte: pgx's prefix check is case-sensitive, so
		// POSTGRES://... falls through to its keyword parser, which ignores
		// the URL query and selects a default host.
		return invalidTransport(setting, "must be an explicit lower-case postgres:// URL")
	}

	rest := value[len(postgresURLPrefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return invalidTransport(setting, "must name a database in the URL path")
	}
	authority := rest[:slash]
	tail := rest[slash:]
	query := ""
	path := tail
	if before, after, ok := strings.Cut(tail, "?"); ok {
		path = before
		query = after
	}

	if err := validatePostgresAuthority(authority, setting); err != nil {
		return err
	}
	database := strings.TrimPrefix(path, "/")
	if database == "" || strings.ContainsRune(database, '/') {
		return invalidTransport(setting, "must name exactly one database in the URL path")
	}
	if !validDatabaseName(database) {
		return invalidTransport(setting, "database name has unsupported characters")
	}
	return validatePostgresQuery(query, setting)
}

// validatePostgresAuthority checks the userinfo and single-host authority of a
// production DSN. pgx splits userinfo at the first '@' before the path, so a
// second literal '@' or any percent encoding in the credentials is refused
// rather than interpreted two ways.
func validatePostgresAuthority(authority, setting string) error {
	if strings.ContainsRune(authority, ',') {
		return invalidTransport(setting, "must name exactly one database host")
	}
	hostPort := authority
	if before, after, ok := strings.Cut(authority, "@"); ok {
		if strings.IndexByte(after, '@') >= 0 {
			return invalidTransport(setting, "must not carry an ambiguous userinfo")
		}
		if err := validatePostgresUserinfo(before, setting); err != nil {
			return err
		}
		hostPort = after
	}
	return validateSingleHostPort(hostPort, setting)
}

// validatePostgresUserinfo accepts only a percent-free user and an optional
// percent-free password. Percent decoding in userinfo can produce '@' or ':'
// inside a credential, and refusing it keeps the credential boundaries
// unambiguous for the driver.
func validatePostgresUserinfo(userinfo, setting string) error {
	if userinfo == "" {
		return invalidTransport(setting, "must not carry empty userinfo")
	}
	user, password, hasPassword := strings.Cut(userinfo, ":")
	if user == "" {
		return invalidTransport(setting, "must not carry empty userinfo")
	}
	if strings.ContainsRune(password, ':') {
		return invalidTransport(setting, "must not carry an ambiguous userinfo")
	}
	if !validUserinfoComponent(user) || (hasPassword && !validUserinfoComponent(password)) {
		return invalidTransport(setting, "userinfo has unsupported characters")
	}
	return nil
}

// validUserinfoComponent accepts the unreserved URL characters only.
func validUserinfoComponent(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_', character == '~':
		default:
			return false
		}
	}
	return true
}

// validatePostgresQuery checks the production DSN query allowlist.
func validatePostgresQuery(query, setting string) error {
	if query == "" {
		return invalidTransport(setting, "must carry exactly one sslmode=verify-full option")
	}
	seen := make(map[string]bool, len(productionPostgresQueryKeys))
	for pair := range strings.SplitSeq(query, "&") {
		if pair == "" {
			return invalidTransport(setting, "has an empty query option")
		}
		before, after, ok0 := strings.Cut(pair, "=")
		if !ok0 {
			return invalidTransport(setting, "has an invalid query option")
		}
		rawKey := before
		rawValue := after
		if strings.IndexByte(rawValue, '=') >= 0 {
			return invalidTransport(setting, "has an invalid query option")
		}
		if rawKey == "" || strings.ContainsRune(rawKey, '%') {
			return invalidTransport(setting, "has an invalid query option key")
		}
		if rawKey != strings.ToLower(rawKey) {
			return invalidTransport(setting, "must spell query option keys in lower case")
		}
		if !productionPostgresQueryKeys[rawKey] {
			return invalidTransport(setting, "carries an unsupported query option")
		}
		if seen[rawKey] {
			return invalidTransport(setting, "repeats a query option")
		}
		seen[rawKey] = true

		value, ok := decodeTransportValue(rawValue)
		if !ok {
			return invalidTransport(setting, "has an invalid query option value")
		}
		switch rawKey {
		case "sslmode":
			if value != "verify-full" {
				return invalidTransport(setting, "must require sslmode=verify-full")
			}
		case "connect_timeout":
			if !validConnectTimeout(value) {
				return invalidTransport(setting, "has an invalid query option value")
			}
		default:
			if value == "" {
				return invalidTransport(setting, "has an empty query option value")
			}
		}
	}
	if !seen["sslmode"] {
		return invalidTransport(setting, "must carry exactly one sslmode=verify-full option")
	}
	return nil
}

// validateProductionNATSURL refuses a production NATS URL that is not a single
// tls:// URL for exactly one broker.
func validateProductionNATSURL(value string) error {
	const setting = "production NATS URL"
	if strings.ContainsRune(value, '#') {
		return invalidTransport(setting, "must not carry a fragment")
	}
	if err := checkTransportBytes(value, setting); err != nil {
		return err
	}
	if len(value) < len(natsTLSURLPrefix) || !strings.EqualFold(value[:len(natsTLSURLPrefix)], natsTLSURLPrefix) {
		return invalidTransport(setting, "must be an explicit tls:// URL")
	}
	rest := value[len(natsTLSURLPrefix):]
	if strings.ContainsRune(rest, ',') {
		return invalidTransport(setting, "must name exactly one broker host")
	}
	if strings.ContainsRune(rest, '@') {
		return invalidTransport(setting, "must not carry connection userinfo")
	}
	end := strings.IndexAny(rest, "/?")
	if end >= 0 {
		rest = rest[:end]
	}
	return validateSingleHostPort(rest, setting)
}

// validateEndpoints requires a single named host for each declared endpoint in
// test and production. It is deliberately weaker than the production transport
// validators: the test environment may use nats:// and needs no sslmode, but an
// empty or comma-list host is refused in both environments.
func (config Config) validateEndpoints() error {
	if err := validateEndpointURL(config.DatabaseURL, "database URL", "postgres"); err != nil {
		return err
	}
	if err := validateEndpointURL(config.NATSURL, "NATS URL", "nats", "tls"); err != nil {
		return err
	}
	if err := validateEndpointURL(config.PublicURL, "public URL", "https"); err != nil {
		return err
	}
	if config.KeyRef == "" {
		return fault.New(fault.InvalidInput, "key reference is empty")
	}
	return nil
}

// validateEndpointURL checks a single-host URL of one of the allowed schemes.
func validateEndpointURL(value, setting string, schemes ...string) error {
	if strings.ContainsRune(value, '#') {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must not carry a fragment", setting))
	}
	if err := checkTransportBytes(value, setting); err != nil {
		return err
	}
	before, after, ok := strings.Cut(value, "://")
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must be an explicit URL", setting))
	}
	scheme := before
	allowed := false
	for _, candidate := range schemes {
		if strings.EqualFold(scheme, candidate) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must be an explicit %s URL", setting, strings.Join(schemes, ":// or ")+"://"))
	}
	rest := after
	end := strings.IndexAny(rest, "/?")
	if end >= 0 {
		rest = rest[:end]
	}
	if strings.ContainsRune(rest, ',') {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must name exactly one host", setting))
	}
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		if strings.IndexByte(rest[at+1:], '@') >= 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s must not carry an ambiguous userinfo", setting))
		}
		rest = rest[at+1:]
	}
	if err := validateSingleHostPort(rest, setting); err != nil {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must name exactly one host", setting))
	}
	return nil
}

// validateSingleHostPort accepts exactly one host with an optional port in
// 1-65535. A Unix-socket path, a comma list, a percent-encoded byte and an
// empty host element are all refused.
func validateSingleHostPort(hostPort, setting string) error {
	if hostPort == "" {
		return invalidTransport(setting, "must name exactly one host")
	}
	if strings.ContainsRune(hostPort, ',') || strings.ContainsRune(hostPort, '%') {
		return invalidTransport(setting, "must name exactly one host")
	}
	host := hostPort
	port := ""
	if strings.HasPrefix(hostPort, "[") {
		end := strings.IndexByte(hostPort, ']')
		if end < 0 {
			return invalidTransport(setting, "must name a valid host")
		}
		if !validIPv6Literal(hostPort[1:end]) {
			return invalidTransport(setting, "must name a valid host")
		}
		after := hostPort[end+1:]
		if after != "" {
			if !strings.HasPrefix(after, ":") || strings.ContainsRune(after[1:], ':') || after[1:] == "" {
				return invalidTransport(setting, "must name a valid port")
			}
			port = after[1:]
		}
	} else {
		if strings.ContainsAny(hostPort, "[]") {
			return invalidTransport(setting, "must name a valid host")
		}
		if colon := strings.IndexByte(hostPort, ':'); colon >= 0 {
			if strings.ContainsRune(hostPort[colon+1:], ':') || colon == len(hostPort)-1 {
				return invalidTransport(setting, "must name a valid port")
			}
			host = hostPort[:colon]
			port = hostPort[colon+1:]
		}
		if !validHostName(host) {
			return invalidTransport(setting, "must name a valid host")
		}
	}
	if port != "" {
		if !validTransportPort(port) {
			return invalidTransport(setting, "must name a valid port")
		}
	}
	return nil
}

// validHostName accepts a DNS name or IPv4 literal: dot-separated non-empty
// labels of ASCII letters, digits, '-' and '_' with no leading or trailing
// hyphen. An absolute path, an empty element and every percent-encoded byte
// are refused.
func validHostName(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_':
			default:
				return false
			}
		}
	}
	return true
}

// validIPv6Literal accepts a conservative bracketed IPv6 literal: hexadecimal
// digits, ':' and '.' only, with at least one ':'.
func validIPv6Literal(literal string) bool {
	if literal == "" || len(literal) > 45 || !strings.ContainsRune(literal, ':') {
		return false
	}
	for index := 0; index < len(literal); index++ {
		character := literal[index]
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		case character == ':', character == '.':
		default:
			return false
		}
	}
	return true
}

// validTransportPort accepts a decimal port in 1-65535.
func validTransportPort(port string) bool {
	if !decimalDigits(port) {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number >= 1 && number <= 65535
}

// validConnectTimeout accepts a decimal second count that pgx can parse as an
// int64. Keeping the bound here means an accepted DSN never trips pgconn's
// "invalid connect_timeout" parse failure.
func validConnectTimeout(value string) bool {
	if !decimalDigits(value) {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

// decimalDigits reports whether value is a non-empty ASCII decimal run.
func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// validDatabaseName accepts one URL path segment of ASCII letters, digits,
// '_', '.' and '-'. A percent encoding is refused so a database name can never
// smuggle '/', '?' or a query separator.
func validDatabaseName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_', character == '.', character == '-':
		default:
			return false
		}
	}
	return true
}

// checkTransportBytes refuses whitespace, control bytes and non-ASCII bytes in
// a raw transport string. pgx trims or tolerates some of these, so a strict
// validator cannot.
func checkTransportBytes(value, setting string) error {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 0x21 || character > 0x7e {
			return invalidTransport(setting, "contains whitespace, control or non-ASCII bytes")
		}
	}
	return nil
}

// decodeTransportValue percent-decodes a query value. It refuses a malformed
// token, a %00 byte, any decoded whitespace, control or non-ASCII byte, and
// never decodes a key.
func decodeTransportValue(raw string) (string, bool) {
	var decoded strings.Builder
	decoded.Grow(len(raw))
	for index := 0; index < len(raw); {
		if raw[index] != '%' {
			if !printableTransportByte(raw[index]) {
				return "", false
			}
			decoded.WriteByte(raw[index])
			index++
			continue
		}
		if index+2 >= len(raw) {
			return "", false
		}
		high, okHigh := hexValue(raw[index+1])
		low, okLow := hexValue(raw[index+2])
		if !okHigh || !okLow {
			return "", false
		}
		value := high<<4 | low
		if value == 0 || !printableTransportByte(value) {
			return "", false
		}
		decoded.WriteByte(value)
		index += 3
	}
	return decoded.String(), true
}

func printableTransportByte(character byte) bool {
	return character >= 0x21 && character <= 0x7e
}

func hexValue(character byte) (byte, bool) {
	switch {
	case character >= '0' && character <= '9':
		return character - '0', true
	case character >= 'a' && character <= 'f':
		return character - 'a' + 10, true
	case character >= 'A' && character <= 'F':
		return character - 'A' + 10, true
	default:
		return 0, false
	}
}

func invalidTransport(setting, reason string) error {
	return fault.New(fault.InvalidInput, fmt.Sprintf("%s %s", setting, reason))
}
