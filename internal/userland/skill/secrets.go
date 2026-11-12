package skill

import "regexp"

// secretPatterns are the credential shapes refused in supplied knowledge. They
// are refusal gates, never redactors, and every pattern is case-insensitive so
// a differently cased header or label cannot slip through.
var secretPatterns = []*regexp.Regexp{
	// A PEM private key header, with any key algorithm between BEGIN and
	// PRIVATE KEY.
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	// An authorization bearer header with a non-empty token.
	regexp.MustCompile(`(?i)authorization:[ \t]*bearer[ \t]+\S+`),
	// An AWS-style access key id.
	regexp.MustCompile(`(?i)\bAKIA[0-9A-Z]{16}\b`),
	// An assignment of a credential label with a non-empty value. The label is
	// matched as the suffix of an identifier because '_' is a word character:
	// GITHUB_TOKEN, AWS_SECRET_ACCESS_KEY and db_password all label a
	// credential yet carry no word boundary of their own. The separator allows
	// the closing quote of a JSON/YAML key, and the value covers bare tokens,
	// quoted values and an unterminated quote through the end of its line.
	regexp.MustCompile(`(?im)(^|[^A-Za-z0-9])([A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key(?:[_-]?id)?|private[_-]?key)(?:[_-](?:access[_-]?key|key|id|token|secret|value))?)(["']?[ \t]*[=:][ \t]*)((?:'[^'\r\n]*[^\s'\r\n][^'\r\n]*'|"[^"\r\n]*[^\s"\r\n][^"\r\n]*"|"[^"\r\n]*$|'[^'\r\n]*$|[^\s"',;][^\s,;]*))`),
}

// ContainsSecret reports whether body carries credential material.
func ContainsSecret(body []byte) bool {
	for _, pattern := range secretPatterns {
		if pattern.Match(body) {
			return true
		}
	}
	return false
}
