package pgtest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// UnitObserver snapshots every declared table before and after one unit runs
// and requires that every inserted, updated or deleted row was whole-row
// asserted by a successful RequireGoldenRow (or RequireRowAbsent for deletes)
// made between ObserveUnit and Finish in the same test.
type UnitObserver struct {
	t        testing.TB
	shard    *Shard
	unit     string
	state    string
	before   map[string]map[string]string
	asserted map[string]bool
	absent   map[string]bool
	finished bool
}

// activeObservers is a stack: tests run sequentially, and RequireGoldenRow
// records the assertion on every observer opened by the asserting test.
var activeObservers []*UnitObserver

// derivedUnitStates records the table set a unit×state actually changed.
var derivedUnitStates = map[string][]string{}

// ObserveUnit snapshots all declared tables and starts observing one unit. The
// observer must be finished: a test that ends with an unfinished observer fails.
func (s *Shard) ObserveUnit(t testing.TB, unit, state string) *UnitObserver {
	t.Helper()
	observer := newUnitObserver(t, s, unit, state, s.snapshotTables(t))
	activeObservers = append(activeObservers, observer)
	t.Cleanup(observer.cleanup)
	return observer
}

// newUnitObserver builds one observer around an already-taken snapshot. Every
// observer owns fresh assertion maps, so no assertion is shared between two.
func newUnitObserver(t testing.TB, s *Shard, unit, state string, before map[string]map[string]string) *UnitObserver {
	return &UnitObserver{
		t:        t,
		shard:    s,
		unit:     unit,
		state:    state,
		before:   before,
		asserted: map[string]bool{},
		absent:   map[string]bool{},
	}
}

// cleanup removes the observer from the active stack and fails a test that
// opened it and never reached Finish (unless the test already failed).
func (o *UnitObserver) cleanup() {
	popObserver(o)
	if !o.finished && !o.t.Failed() {
		o.t.Errorf("unit×state %s.%s was observed but never finished", o.unit, o.state)
	}
}

// observes reports whether an assertion made by t belongs to this observer: the
// observer's own test or one of its subtests. Another test's assertions never do.
func (o *UnitObserver) observes(t testing.TB) bool {
	if t == o.t {
		return true
	}
	return strings.HasPrefix(t.Name(), o.t.Name()+"/")
}

// RequireRowAbsent asserts that no row matches key and records the absence for
// every observer of the asserting test, so a deleted row can be proved removed.
func (s *Shard) RequireRowAbsent(t testing.TB, ctx context.Context, db *pgdriver.DB, scope tenancy.Scope,
	table string, key map[string]any) {
	t.Helper()
	count, err := s.countRow(ctx, db, scope, table, key)
	if err != nil {
		t.Fatalf("require row absent %s: %v", table, err)
	}
	if count != 0 {
		t.Fatalf("require row absent %s: %d rows match %v", table, count, key)
	}
	if id, ok := key["id"]; ok {
		recordRowAbsence(t, table, fmt.Sprint(id))
	}
}

// recordRowAbsence marks one table+id as proved absent on every observer of t.
func recordRowAbsence(t testing.TB, table, id string) {
	for _, observer := range activeObservers {
		if observer.observes(t) {
			observer.absent[table+"\x00"+id] = true
		}
	}
}

// recordGoldenAssertion marks one successfully compared table+id on every
// observer of the asserting test.
func recordGoldenAssertion(t testing.TB, table, id string) {
	for _, observer := range activeObservers {
		if observer.observes(t) {
			observer.asserted[table+"\x00"+id] = true
		}
	}
}

// Finish snapshots again, checks every changed row, registers the unit×state
// and records the derived table set. A changed row without an assertion fails.
func (o *UnitObserver) Finish(expected map[string]int) {
	o.t.Helper()
	after := o.shard.snapshotTables(o.t)
	if err := o.finishWith(after, expected); err != nil {
		o.t.Fatal(err)
	}
}

// finishWith is Finish without the database: it diffs the two snapshots, checks
// assertion coverage and counts, registers the unit×state and pops the observer.
func (o *UnitObserver) finishWith(after map[string]map[string]string, expected map[string]int) error {
	if o.finished {
		return fmt.Errorf("unit×state %s.%s finished twice", o.unit, o.state)
	}
	o.finished = true
	changed := diffUnitSnapshots(o.before, after)
	if missing := unitChangeGaps(changed, o.asserted, o.absent); len(missing) > 0 {
		return fmt.Errorf("unit×state %s.%s changed rows without whole-row assertions: %v", o.unit, o.state, missing)
	}
	tables := make([]string, 0, len(changed))
	counts := make(map[string]int, len(changed))
	for table, rows := range changed {
		tables = append(tables, table)
		counts[table] = len(rows)
	}
	sort.Strings(tables)
	if mismatched := unitChangeCountMismatch(counts, expected); len(mismatched) > 0 {
		return fmt.Errorf("unit×state %s.%s changed rows %v; expected-set mismatch: %v", o.unit, o.state, counts, mismatched)
	}
	derivedUnitStates[o.unit+"."+o.state] = tables
	popObserver(o)
	return nil
}

func popObserver(o *UnitObserver) {
	for index, activeObserver := range slices.Backward(activeObservers) {
		if activeObserver == o {
			activeObservers = append(activeObservers[:index], activeObservers[index+1:]...)
			return
		}
	}
}

// diffUnitSnapshots returns the changed rows (inserted, updated or deleted) per
// table. It is pure so the observer logic is testable without a database.
func diffUnitSnapshots(before, after map[string]map[string]string) map[string]map[string]bool {
	changed := map[string]map[string]bool{}
	mark := func(table, id string) {
		if changed[table] == nil {
			changed[table] = map[string]bool{}
		}
		changed[table][id] = true
	}
	for table, rows := range before {
		for id, hash := range rows {
			if afterHash, ok := after[table][id]; !ok || afterHash != hash {
				mark(table, id)
			}
		}
	}
	for table, rows := range after {
		for id, hash := range rows {
			if beforeHash, ok := before[table][id]; !ok || beforeHash != hash {
				mark(table, id)
			}
		}
	}
	return changed
}

// unitChangeCountMismatch compares the derived changed-row counts per table
// with the explicitly expected counts. A dropped row changes the count even when
// its table still has other rows, and an extra row changes it too.
func unitChangeCountMismatch(derived, expected map[string]int) []string {
	var mismatch []string
	for table, want := range expected {
		if got, ok := derived[table]; !ok {
			mismatch = append(mismatch, "missing "+table)
		} else if got != want {
			mismatch = append(mismatch, fmt.Sprintf("%s count %d want %d", table, got, want))
		}
	}
	for table := range derived {
		if _, ok := expected[table]; !ok {
			mismatch = append(mismatch, "unexpected "+table)
		}
	}
	sort.Strings(mismatch)
	return mismatch
}

// unitChangeGaps lists changed rows that are neither whole-row asserted nor
// explicitly asserted absent.
func unitChangeGaps(changed map[string]map[string]bool, asserted, absent map[string]bool) []string {
	var gaps []string
	for table, rows := range changed {
		ids := make([]string, 0, len(rows))
		for id := range rows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			key := table + "\x00" + id
			if asserted[key] || absent[key] {
				continue
			}
			gaps = append(gaps, table+"/"+id)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// DerivedUnitStates returns a copy of the observed unit×state registry.
func DerivedUnitStates() map[string][]string {
	states := make(map[string][]string, len(derivedUnitStates))
	for key, tables := range derivedUnitStates {
		states[key] = append([]string(nil), tables...)
	}
	return states
}

// snapshotTables reads id and a content hash of every row in every declared
// table through the administrator connection.
func (s *Shard) snapshotTables(t testing.TB) map[string]map[string]string {
	t.Helper()
	snapshot := map[string]map[string]string{}
	for _, table := range store.SchemaDesign().Tables {
		rows, err := QueryStringsN(context.Background(), s.Admin(), 2,
			`SELECT id::text, md5(t::text) FROM `+quoteGoldenIdent(table.Name)+` t`)
		if err != nil {
			t.Fatalf("observer snapshot %s: %v", table.Name, err)
		}
		tableRows, err := indexSnapshotRows(table.Name, rows)
		if err != nil {
			t.Fatalf("observer snapshot: %v", err)
		}
		snapshot[table.Name] = tableRows
	}
	return snapshot
}

// indexSnapshotRows maps row id to content hash and refuses a duplicate id: two
// rows sharing an id would silently overwrite each other in the snapshot and
// hide a change to one of them.
func indexSnapshotRows(table string, rows [][]string) (map[string]string, error) {
	indexed := make(map[string]string, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("table %s: snapshot row has %d columns; want 2", table, len(row))
		}
		if _, duplicate := indexed[row[0]]; duplicate {
			return nil, fmt.Errorf("table %s: duplicate row id %q in snapshot", table, row[0])
		}
		indexed[row[0]] = row[1]
	}
	return indexed, nil
}

func quoteGoldenIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// countRow counts rows matching the key in one table inside the given scope.
func (s *Shard) countRow(ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, table string, key map[string]any) (int64, error) {
	names := make([]string, 0, len(key))
	for name := range key {
		names = append(names, name)
	}
	sort.Strings(names)
	predicates := make([]string, 0, len(names))
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, key[name])
		predicates = append(predicates, fmt.Sprintf("%s = $%d", quoteGoldenIdent(name), len(args)))
	}
	query := fmt.Sprintf("SELECT count(*)::text FROM %s WHERE %s", quoteGoldenIdent(table), strings.Join(predicates, " AND "))
	rows, err := ScopedQuery(ctx, db, scope, 1, query, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("count returned %d rows", len(rows))
	}
	var count int64
	if _, err := fmt.Sscan(rows[0][0], &count); err != nil {
		return 0, err
	}
	return count, nil
}
