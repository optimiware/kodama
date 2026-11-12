package memory

import (
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/shell/store"
)

// uniqueRule is one declared uniqueness rule the adapter checks against the
// staged rows of its table. It is derived from the table declaration,
// never hand-maintained.
type uniqueRule struct {
	name             string
	columns          []string
	nullsNotDistinct bool
}

// uniqueRules derives the rules one table's writes are checked against: every
// declared unique constraint and every declared unique index without a
// predicate. A rule whose columns include the whole primary key is implied by
// the primary key the adapter already enforces and is dropped. A partial unique
// index is not mirrored: its predicate is SQL text this adapter does not
// evaluate.
func uniqueRules(table store.Table) []uniqueRule {
	rules := make([]uniqueRule, 0, len(table.Unique))
	add := func(name string, columns []string, nullsNotDistinct bool) {
		if len(columns) == 0 || impliedByPrimaryKey(table.PrimaryKey, columns) {
			return
		}
		rules = append(rules, uniqueRule{
			name:             name,
			columns:          append([]string(nil), columns...),
			nullsNotDistinct: nullsNotDistinct,
		})
	}
	for _, unique := range table.Unique {
		add(unique.Name, unique.Columns, unique.NullsNotDistinct)
	}
	for _, index := range table.Indexes {
		if index.Unique && index.Predicate == "" {
			add(index.Name, index.Columns, false)
		}
	}
	return rules
}

// impliedByPrimaryKey reports whether a column set contains every primary-key
// column, so two distinct rows can never be equal on it.
func impliedByPrimaryKey(primaryKey, columns []string) bool {
	if len(primaryKey) == 0 {
		return false
	}
	for _, name := range primaryKey {
		if !slices.Contains(columns, name) {
			return false
		}
	}
	return true
}

// touchedBy reports whether an update's value columns include one of the rule's
// columns. An update that sets none of them cannot change the row's key.
func (rule uniqueRule) touchedBy(values map[string]any) bool {
	for _, name := range rule.columns {
		if _, ok := values[name]; ok {
			return true
		}
	}
	return false
}

// keyed reports whether a row takes part in the rule at all. A NULL in any
// constrained column makes the row distinct from every other row, unless the
// rule declares NULLS NOT DISTINCT.
func (rule uniqueRule) keyed(values map[string]any) bool {
	if rule.nullsNotDistinct {
		return true
	}
	for _, name := range rule.columns {
		if values[name] == nil {
			return false
		}
	}
	return true
}

// equal reports whether two rows carry the same key. valuesEqual treats NULL as
// equal only to NULL, which a NULLS NOT DISTINCT rule needs; an ordinary rule
// never reaches here with a NULL on the written side.
func (rule uniqueRule) equal(left, right map[string]any) bool {
	for _, name := range rule.columns {
		if !valuesEqual(left[name], right[name]) {
			return false
		}
	}
	return true
}

// checkUnique asserts the declared uniqueness rules of one table for the row an
// insert or update just staged. It runs after each such write, the point at
// which PostgreSQL checks a constraint that is not deferrable, and compares the
// written row with every other staged row of the table in any scope. The
// refusal names the constraint and never a row value.
func checkUnique(staged *state, write plannedWrite) error {
	rules := write.meta.unique
	if len(rules) == 0 {
		return nil
	}
	rows := staged.rows[write.table]
	written := rows[write.identity]
	for _, rule := range rules {
		if !write.insert && !rule.touchedBy(write.values) {
			continue
		}
		if !rule.keyed(written) {
			continue
		}
		for identity, values := range rows {
			if identity != write.identity && rule.equal(written, values) {
				return faultConflict(fmt.Sprintf("a written row violates the unique constraint %q", rule.name))
			}
		}
	}
	return nil
}
