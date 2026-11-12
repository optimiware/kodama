//go:build component

package postgrescomponent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// livePolicy is one row of pg_policies for the public schema.
type livePolicy struct {
	Name       string
	Table      string
	Command    string
	Permissive string
	Roles      []string
	Using      string
	WithCheck  string
}

// readLivePolicies reads every public-schema policy with its command,
// permissiveness, roles, USING and WITH CHECK text.
func readLivePolicies(ctx context.Context, t *testing.T, shard *pgtest.Shard) []livePolicy {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 7,
		`SELECT policyname, tablename, cmd, permissive, roles::text, COALESCE(qual, ''), COALESCE(with_check, '')
		 FROM pg_policies WHERE schemaname = 'public' ORDER BY policyname`)
	if err != nil {
		t.Fatalf("read live policies: %v", err)
	}
	out := make([]livePolicy, 0, len(rows))
	for _, row := range rows {
		out = append(out, livePolicy{
			Name:       row[0],
			Table:      row[1],
			Command:    row[2],
			Permissive: row[3],
			Roles:      splitRoleList(row[4]),
			Using:      row[5],
			WithCheck:  row[6],
		})
	}
	return out
}

func splitRoleList(text string) []string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "{")
	text = strings.TrimSuffix(text, "}")
	if text == "" {
		return nil
	}
	parts := strings.Split(text, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// declaredPolicyFailures compares every declared policy to the live catalog
// per role, table and command: the exact key set, the single declared role, the
// PERMISSIVE posture, and the USING/WITH CHECK predicate descriptors mapped to
// the live expression. It also rejects a policy aimed at the application role
// and any unexpected PUBLIC policy, because either would widen the roles under
// test beyond the declaration.
func declaredPolicyFailures(ctx context.Context, t *testing.T, shard *pgtest.Shard, declared []schemaPolicy) []string {
	t.Helper()
	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	live := readLivePolicies(ctx, t, shard)

	systemRoles := map[string]bool{
		"kodama_controller": true,
		"kodama_publisher":  true,
		"kodama_auth":       true,
	}
	for _, policy := range live {
		for _, role := range policy.Roles {
			lower := strings.ToLower(role)
			if lower == "kodama_runtime" {
				fail("policy %q targets kodama_runtime; the application role must rely only on the PUBLIC scope policies", policy.Name)
			}
			if lower == "public" {
				if policy.Name != policy.Table+"_scope" {
					fail("unexpected PUBLIC policy %q on table %q; only <table>_scope is declared", policy.Name, policy.Table)
				}
				if !strings.EqualFold(policy.Command, "ALL") {
					fail("PUBLIC scope policy %q command = %q; want ALL", policy.Name, policy.Command)
				}
				if !strings.Contains(normalizeSQL(policy.Using), "current_setting('kodama.tenant_id'") {
					fail("PUBLIC scope policy %q USING = %q; want the transaction tenant scope", policy.Name, policy.Using)
				}
			}
		}
	}

	byKey := make(map[string][]livePolicy)
	for _, policy := range live {
		for _, role := range policy.Roles {
			if !systemRoles[strings.ToLower(role)] {
				continue
			}
			key := strings.ToLower(role) + "|" + strings.ToLower(policy.Table) + "|" + strings.ToLower(policy.Command)
			byKey[key] = append(byKey[key], policy)
		}
	}

	declaredKeys := make(map[string]bool, len(declared))
	for _, want := range declared {
		key := strings.ToLower(want.Role) + "|" + strings.ToLower(want.Table) + "|" + strings.ToLower(want.Command)
		declaredKeys[key] = true
		rows := byKey[key]
		switch {
		case len(rows) == 0:
			fail("declared policy %s|%s|%s is missing from the live catalog", want.Role, want.Table, want.Command)
			continue
		case len(rows) > 1:
			fail("declared policy %s|%s|%s appears %d times; want exactly one", want.Role, want.Table, want.Command, len(rows))
			continue
		}
		row := rows[0]
		if !sameSet(row.Roles, []string{want.Role}) {
			fail("policy %q roles = %v; want exactly [%s]", row.Name, row.Roles, want.Role)
		}
		if !strings.EqualFold(row.Permissive, "PERMISSIVE") {
			fail("policy %q permissive = %q; want PERMISSIVE", row.Name, row.Permissive)
		}
		// PostgreSQL evaluates USING and WITH CHECK separately by command:
		// INSERT admits by WITH CHECK only, SELECT and DELETE by USING only,
		// and UPDATE by both. Compare only the clause the command defines.
		switch strings.ToLower(want.Command) {
		case "insert":
			if !predicateMatches(want.WithCheck, row.WithCheck, want.Column, want.Values) {
				fail("policy %q WITH CHECK = %q; want descriptor %q on column %q values %v",
					row.Name, row.WithCheck, want.WithCheck, want.Column, want.Values)
			}
		case "select", "delete":
			if !predicateMatches(want.Using, row.Using, want.Column, want.Values) {
				fail("policy %q USING = %q; want descriptor %q on column %q values %v",
					row.Name, row.Using, want.Using, want.Column, want.Values)
			}
			if strings.TrimSpace(row.WithCheck) != "" {
				fail("policy %q has WITH CHECK %q; %s policies admit by USING only",
					row.Name, row.WithCheck, strings.ToUpper(want.Command))
			}
		case "update":
			if !predicateMatches(want.Using, row.Using, want.Column, want.Values) {
				fail("policy %q USING = %q; want descriptor %q on column %q values %v",
					row.Name, row.Using, want.Using, want.Column, want.Values)
			}
			if !predicateMatches(want.WithCheck, row.WithCheck, want.Column, want.Values) {
				fail("policy %q WITH CHECK = %q; want descriptor %q on column %q values %v",
					row.Name, row.WithCheck, want.WithCheck, want.Column, want.Values)
			}
		case "all":
			if !predicateMatches(want.Using, row.Using, want.Column, want.Values) {
				fail("policy %q USING = %q; want descriptor %q on column %q values %v",
					row.Name, row.Using, want.Using, want.Column, want.Values)
			}
			if want.WithCheck != "" && !predicateMatches(want.WithCheck, row.WithCheck, want.Column, want.Values) {
				fail("policy %q WITH CHECK = %q; want descriptor %q on column %q values %v",
					row.Name, row.WithCheck, want.WithCheck, want.Column, want.Values)
			}
		default:
			fail("declared policy %s|%s has unsupported command %q", want.Role, want.Table, want.Command)
		}
	}
	for key, rows := range byKey {
		if declaredKeys[key] {
			continue
		}
		fail("undeclared policy %s appears %d times for a system role", key, len(rows))
	}
	return failures
}

var (
	currentSettingPattern = regexp.MustCompile(`^current_setting\('([^']+)',\s*true\)$`)
	columnPattern         = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	numberPattern         = regexp.MustCompile(`^-?[0-9]+$`)
)

// predicateMatches reports whether live is exactly the declared portable
// predicate descriptor. It parses the small conjunction vocabulary the
// renderer emits and compares canonical atoms, so a widened, narrowed or
// re-associated predicate fails instead of matching a substring.
func predicateMatches(descriptor, live, column string, values []string) bool {
	live = normalizeSQL(live)
	if live == "" {
		return false
	}
	want, ok := expectedPredicate(descriptor, column, values)
	if !ok {
		return false
	}
	parts := splitTopLevelAnd(stripOuterParens(live))
	got := make([]string, 0, len(parts))
	for _, part := range parts {
		atom, ok := parseAtom(part)
		if !ok {
			return false
		}
		got = append(got, atom)
	}
	return sameSet(got, want)
}

// expectedPredicate maps one portable descriptor to the canonical conjunction
// it must render as.
func expectedPredicate(descriptor, column string, values []string) ([]string, bool) {
	switch descriptor {
	case "any":
		return []string{"TRUE"}, true
	case "scope_matches_txn_scope":
		return []string{
			"EQ:COL:tenant_id:SETTING:kodama.tenant_id",
			"EQ:COL:project_id:SETTING:kodama.project_id",
		}, true
	case "exact_row":
		if column == "" {
			return nil, false
		}
		return []string{"EQ:COL:" + column + ":SETTING:kodama.credential_id"}, true
	case "column_in_set":
		if column == "" || len(values) == 0 {
			return nil, false
		}
		sorted := append([]string(nil), values...)
		sort.Strings(sorted)
		return []string{"IN:COL:" + column + ":" + strings.Join(sorted, ",")}, true
	default:
		return nil, false
	}
}

// normalizeSQL removes the rendering noise PostgreSQL adds to a stored
// expression (lower case, explicit text casts, catalog qualification and
// whitespace) without changing its structure.
func normalizeSQL(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "::text", "")
	value = strings.ReplaceAll(value, "pg_catalog.", "")
	return strings.Join(strings.Fields(value), " ")
}

func stripOuterParens(value string) string {
	for {
		value = strings.TrimSpace(value)
		if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' || !outerParensMatch(value) {
			return value
		}
		value = value[1 : len(value)-1]
	}
}

// outerParensMatch reports whether the first parenthesis closes exactly at the
// final byte, ignoring parentheses inside single-quoted literals.
func outerParensMatch(value string) bool {
	depth := 0
	inQuote := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if inQuote {
			if char == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch char {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && index != len(value)-1 {
				return false
			}
		}
	}
	return depth == 0 && !inQuote
}

func splitTopLevelAnd(value string) []string {
	parts := make([]string, 0, 2)
	depth := 0
	inQuote := false
	start := 0
	for index := 0; index < len(value); index++ {
		char := value[index]
		if inQuote {
			if char == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch char {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && !inQuote && strings.HasPrefix(value[index:], " and ") {
			parts = append(parts, strings.TrimSpace(value[start:index]))
			index += len(" and ") - 1
			start = index + 1
		}
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts
}

// parseAtom canonicalizes one boolean atom: a literal true/false, a commutative
// equality, or a membership set.
func parseAtom(value string) (string, bool) {
	value = stripOuterParens(strings.TrimSpace(value))
	switch value {
	case "true":
		return "TRUE", true
	case "false":
		return "FALSE", true
	}
	if left, right, ok := splitTopLevelEquals(value); ok {
		left = stripOuterParens(strings.TrimSpace(left))
		right = stripOuterParens(strings.TrimSpace(right))
		if values, ok := parseMembershipSet(right); ok {
			column, ok := parseColumn(left)
			if !ok {
				return "", false
			}
			return "IN:COL:" + column + ":" + strings.Join(values, ","), true
		}
		leftOperand, ok := parseOperand(left)
		if !ok {
			return "", false
		}
		rightOperand, ok := parseOperand(right)
		if !ok {
			return "", false
		}
		if leftOperand > rightOperand {
			leftOperand, rightOperand = rightOperand, leftOperand
		}
		return "EQ:" + leftOperand + ":" + rightOperand, true
	}
	if left, right, ok := splitTopLevelKeyword(value, " in "); ok {
		column, ok := parseColumn(stripOuterParens(strings.TrimSpace(left)))
		if !ok {
			return "", false
		}
		right = stripOuterParens(strings.TrimSpace(right))
		if !strings.HasPrefix(right, "(") || !strings.HasSuffix(right, ")") {
			return "", false
		}
		values, ok := parseStringList(right[1 : len(right)-1])
		if !ok {
			return "", false
		}
		return "IN:COL:" + column + ":" + strings.Join(values, ","), true
	}
	return "", false
}

// splitTopLevelEquals splits on the single top-level equality operator. A
// comparison operator such as >= or <> is not an equality and is rejected.
func splitTopLevelEquals(value string) (string, string, bool) {
	depth := 0
	inQuote := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if inQuote {
			if char == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch char {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case '=':
			if depth != 0 {
				continue
			}
			if index > 0 && strings.ContainsRune("<>=!", rune(value[index-1])) {
				return "", "", false
			}
			if index+1 < len(value) && value[index+1] == '=' {
				return "", "", false
			}
			return value[:index], value[index+1:], true
		}
	}
	return "", "", false
}

func splitTopLevelKeyword(value, keyword string) (string, string, bool) {
	depth := 0
	inQuote := false
	for index := 0; index < len(value); index++ {
		char := value[index]
		if inQuote {
			if char == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch char {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && !inQuote && strings.HasPrefix(value[index:], keyword) {
			return value[:index], value[index+len(keyword):], true
		}
	}
	return "", "", false
}

// parseMembershipSet parses PostgreSQL's stored "ANY (ARRAY[...])" form.
func parseMembershipSet(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "any") {
		return nil, false
	}
	value = strings.TrimSpace(value[len("any"):])
	if !strings.HasPrefix(value, "(") || !strings.HasSuffix(value, ")") {
		return nil, false
	}
	value = strings.TrimSpace(value[1 : len(value)-1])
	if !strings.HasPrefix(value, "array") {
		return nil, false
	}
	value = strings.TrimSpace(value[len("array"):])
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false
	}
	return parseStringList(value[1 : len(value)-1])
}

func parseStringList(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	values := make([]string, 0, 2)
	depth := 0
	inQuote := false
	start := 0
	for index := 0; index < len(value); index++ {
		char := value[index]
		if inQuote {
			if char == '\'' {
				if index+1 < len(value) && value[index+1] == '\'' {
					index++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch char {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth != 0 {
				continue
			}
			literal, ok := parseStringLiteral(strings.TrimSpace(value[start:index]))
			if !ok {
				return nil, false
			}
			values = append(values, literal)
			start = index + 1
		}
	}
	literal, ok := parseStringLiteral(strings.TrimSpace(value[start:]))
	if !ok {
		return nil, false
	}
	values = append(values, literal)
	sort.Strings(values)
	unique := values[:0]
	for index, item := range values {
		if index > 0 && item == values[index-1] {
			continue
		}
		unique = append(unique, item)
	}
	return unique, true
}

func parseStringLiteral(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' {
		return "", false
	}
	inner := value[1 : len(value)-1]
	inner = strings.ReplaceAll(inner, "''", "'")
	if strings.Contains(inner, "'") {
		return "", false
	}
	return inner, true
}

func parseColumn(value string) (string, bool) {
	operand, ok := parseOperand(value)
	if !ok || !strings.HasPrefix(operand, "COL:") {
		return "", false
	}
	return strings.TrimPrefix(operand, "COL:"), true
}

func parseOperand(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if matches := currentSettingPattern.FindStringSubmatch(value); matches != nil {
		return "SETTING:" + matches[1], true
	}
	if columnPattern.MatchString(value) {
		return "COL:" + value, true
	}
	if numberPattern.MatchString(value) {
		return "NUM:" + value, true
	}
	if literal, ok := parseStringLiteral(value); ok {
		return "LIT:" + literal, true
	}
	return "", false
}
