package git

import (
	"fmt"
	"strings"
)

// ParseConfig parses one tenant `admin/conf/git.conf`. The format is ordered
// rules per repository plus `option resource = <resource-id>` and
// `option approve.<ref> = <role>` lines. Comments and blank lines are
// insignificant. Unknown permissions, principals, malformed lines, glob or
// unanchored refs and duplicate repositories or duplicate `option resource`
// declarations refuse.
func ParseConfig(data []byte) (Config, error) {
	config := Config{policies: make(map[string]RepositoryPolicy)}
	current := ""
	for index, raw := range strings.Split(string(data), "\n") {
		lineNumber := index + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "repo "):
			name := strings.TrimSpace(strings.TrimPrefix(line, "repo "))
			if !validRepositoryName(name) {
				return Config{}, fmt.Errorf("line %d: invalid repository name %q", lineNumber, name)
			}
			if _, duplicate := config.policies[name]; duplicate {
				return Config{}, fmt.Errorf("line %d: repository %q is declared twice", lineNumber, name)
			}
			config.policies[name] = RepositoryPolicy{Name: name}
			config.order = append(config.order, name)
			current = name
		case strings.HasPrefix(line, "option "):
			if current == "" {
				return Config{}, fmt.Errorf("line %d: option before a repository", lineNumber)
			}
			policy := config.policies[current]
			if err := applyOption(lineNumber, current, strings.TrimSpace(strings.TrimPrefix(line, "option ")), &policy); err != nil {
				return Config{}, err
			}
			config.policies[current] = policy
		default:
			if current == "" {
				return Config{}, fmt.Errorf("line %d: rule before a repository", lineNumber)
			}
			rule, err := parseRule(lineNumber, line)
			if err != nil {
				return Config{}, err
			}
			policy := config.policies[current]
			policy.Rules = append(policy.Rules, rule)
			config.policies[current] = policy
		}
	}
	return config, nil
}

// parseRule parses one `<permission> [<ref>] = <principal>...` line.
func parseRule(lineNumber int, line string) (Rule, error) {
	parts := strings.Split(line, "=")
	if len(parts) != 2 {
		return Rule{}, fmt.Errorf("line %d: rule must contain exactly one '='", lineNumber)
	}
	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) < 1 || len(left) > 2 {
		return Rule{}, fmt.Errorf("line %d: rule needs a permission and at most one ref", lineNumber)
	}
	permission := Permission(left[0])
	if !permission.Valid() {
		return Rule{}, fmt.Errorf("line %d: unknown permission %q", lineNumber, left[0])
	}
	ref := ""
	if len(left) == 2 {
		ref = left[1]
		if !validRef(ref) {
			return Rule{}, fmt.Errorf("line %d: invalid ref %q", lineNumber, ref)
		}
	}
	if len(right) == 0 {
		return Rule{}, fmt.Errorf("line %d: rule has no principals", lineNumber)
	}
	principals := make([]PrincipalRef, 0, len(right))
	for _, value := range right {
		principal := PrincipalRef(value)
		if !principal.Valid() {
			return Rule{}, fmt.Errorf("line %d: unknown principal %q", lineNumber, value)
		}
		principals = append(principals, principal)
	}
	return Rule{Line: lineNumber, Permission: permission, Ref: ref, Principals: principals}, nil
}

// applyOption parses one `resource = <resource-id>` or `approve.<ref> = <role>`
// option body and updates policy in place.
func applyOption(lineNumber int, repository, body string, policy *RepositoryPolicy) error {
	parts := strings.Split(body, "=")
	if len(parts) != 2 {
		return fmt.Errorf("line %d: option must contain exactly one '='", lineNumber)
	}
	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) != 1 || len(right) != 1 {
		return fmt.Errorf("line %d: option needs one key and one value", lineNumber)
	}
	if left[0] == "resource" {
		resourceID := right[0]
		if !validJournalID(resourceID) {
			return fmt.Errorf("line %d: invalid resource id %q", lineNumber, resourceID)
		}
		if policy.ResourceID != "" {
			return fmt.Errorf("line %d: repository %q declares option resource twice", lineNumber, repository)
		}
		policy.ResourceID = resourceID
		return nil
	}
	approval, err := parseApproval(lineNumber, body)
	if err != nil {
		return err
	}
	policy.Approvals = append(policy.Approvals, approval)
	return nil
}

// parseApproval parses one `approve.<ref> = <role>` option body.
func parseApproval(lineNumber int, body string) (ApprovalRule, error) {
	parts := strings.Split(body, "=")
	if len(parts) != 2 {
		return ApprovalRule{}, fmt.Errorf("line %d: approval option must contain exactly one '='", lineNumber)
	}
	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) != 1 || len(right) != 1 {
		return ApprovalRule{}, fmt.Errorf("line %d: approval option needs one ref and one role", lineNumber)
	}
	const prefix = "approve."
	if !strings.HasPrefix(left[0], prefix) {
		return ApprovalRule{}, fmt.Errorf("line %d: approval option must start with %q", lineNumber, prefix)
	}
	ref := strings.TrimPrefix(left[0], prefix)
	if !validRef(ref) {
		return ApprovalRule{}, fmt.Errorf("line %d: invalid approval ref %q", lineNumber, ref)
	}
	role := right[0]
	if !validHumanRoleName(role) {
		return ApprovalRule{}, fmt.Errorf("line %d: unknown approval role %q", lineNumber, role)
	}
	return ApprovalRule{Line: lineNumber, Ref: ref, Role: role}, nil
}

// validRepositoryName reports whether value is a tenant-relative repository
// name: slash-separated lowercase segments without glob, parent or space.
func validRepositoryName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	if strings.ContainsAny(value, "*?[]\\ ") || strings.Contains(value, "..") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" {
			return false
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_', character == '.':
			default:
				return false
			}
		}
	}
	return true
}

// Authorize evaluates one attempt against the ordered rules: the first
// ref/principal match whose permission covers the attempt allows, a matching
// deny refuses and no match refuses.
func (policy RepositoryPolicy) Authorize(request AccessRequest) (Decision, error) {
	if err := request.Validate(); err != nil {
		return DecisionDeny, err
	}
	for _, rule := range policy.Rules {
		if !ruleMatchesRef(rule.Ref, request.Ref) {
			continue
		}
		if !ruleMatchesIdentity(rule.Principals, request.Identity) {
			continue
		}
		if rule.Permission == PermissionDeny {
			return DecisionDeny, nil
		}
		if rule.Permission.covers(request.Update) {
			return DecisionAllow, nil
		}
	}
	return DecisionDeny, nil
}

// AuthorizeRead evaluates one repository-level read attempt against the
// repository's local ordered rules. Only a rule that covers the whole
// repository (a ref-less rule or the all-refs `refs/` prefix) decides, so a
// ref-scoped deny alone never hides the repository: `- refs/heads/private =
// @viewer` followed by `R = @viewer` permits the repository read. `R`, `RW` and
// `RW+` cover a read; a matching deny refuses and no matching repository rule
// refuses. A read never uses a per-ref rule and there is no `git.ref.read`
// resource operation; `git.ref.update` verification/approval remains the update
// path.
func (policy RepositoryPolicy) AuthorizeRead(request ReadRequest) (Decision, error) {
	if err := request.Validate(); err != nil {
		return DecisionDeny, err
	}
	for _, rule := range policy.Rules {
		if !ruleCoversRepository(rule.Ref) {
			continue
		}
		if !ruleMatchesIdentity(rule.Principals, request.Identity) {
			continue
		}
		if rule.Permission == PermissionDeny {
			return DecisionDeny, nil
		}
		if rule.Permission.covers(UpdateRead) {
			return DecisionAllow, nil
		}
	}
	return DecisionDeny, nil
}

// ruleCoversRepository reports whether a rule ref covers the whole repository.
// A ref-less rule covers every ref; the `refs/` prefix is the all-refs prefix.
// Any narrower ref (exact or segment prefix) is a per-ref rule and never
// decides a repository-level read.
func ruleCoversRepository(ruleRef string) bool {
	return ruleRef == "" || ruleRef == "refs/"
}

// ApprovalFor returns the human role required to update ref, if an approval
// option matches exactly or by a ref prefix ending in "/".
func (policy RepositoryPolicy) ApprovalFor(ref string) (string, bool) {
	for _, approval := range policy.Approvals {
		if ruleMatchesRef(approval.Ref, ref) {
			return approval.Role, true
		}
	}
	return "", false
}

// ruleMatchesRef reports whether a rule ref covers the attempt ref. An empty
// rule ref covers every ref; a ref ending in "/" is a segment prefix.
func ruleMatchesRef(ruleRef, ref string) bool {
	if ruleRef == "" {
		return true
	}
	if strings.HasSuffix(ruleRef, "/") {
		return strings.HasPrefix(ref, ruleRef)
	}
	return ruleRef == ref
}

// ruleMatchesIdentity reports whether any principal matches the identity.
func ruleMatchesIdentity(principals []PrincipalRef, identity Identity) bool {
	for _, principal := range principals {
		if principalMatches(principal, identity) {
			return true
		}
	}
	return false
}

// principalMatches reports whether one principal reference matches.
func principalMatches(principal PrincipalRef, identity Identity) bool {
	value := string(principal)
	switch value {
	case "@all":
		return true
	case "@execution":
		return identity.Execution
	}
	if strings.HasPrefix(value, "@") {
		if identity.Execution || identity.ServiceID != "" {
			return false
		}
		role := strings.TrimPrefix(value, "@")
		for _, candidate := range identity.Roles {
			if candidate == role {
				return true
			}
		}
		return false
	}
	return identity.ServiceID != "" && identity.ServiceID == value
}
