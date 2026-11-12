package memory

import (
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
)

// tableMeta is the memory adapter's indexed view of one declared schema table.
// It is derived from store.SchemaDesign, never hand-maintained, so the adapter's
// scope, version and lock-order decisions cannot drift from the shared schema.
type tableMeta struct {
	table           store.Table
	columnOrder     []string
	columns         map[string]store.Column
	hasTenant       bool
	hasProject      bool
	projectRequired bool
	hasVersion      bool
	unique          []uniqueRule
}

func (meta *tableMeta) hasColumn(name string) bool {
	_, ok := meta.columns[name]
	return ok
}

// schemaIndex is the deterministic lookup data built once from the declared
// schema design.
type schemaIndex struct {
	tables     map[string]*tableMeta
	lockOrder  map[string]int
	writeRanks map[string]float64
}

var schema = buildSchemaIndex()

func buildSchemaIndex() schemaIndex {
	design := store.SchemaDesign()
	index := schemaIndex{
		tables:     make(map[string]*tableMeta, len(design.Tables)),
		lockOrder:  make(map[string]int, len(design.LockOrder)),
		writeRanks: make(map[string]float64, len(design.Tables)),
	}
	for position, name := range design.LockOrder {
		index.lockOrder[name] = position
	}
	for _, table := range design.Tables {
		meta := &tableMeta{
			table:       table,
			columnOrder: make([]string, 0, len(table.Columns)),
			columns:     make(map[string]store.Column, len(table.Columns)),
		}
		for _, column := range table.Columns {
			meta.columnOrder = append(meta.columnOrder, column.Name)
			meta.columns[column.Name] = column
		}
		meta.hasTenant = meta.hasColumn("tenant_id")
		meta.hasProject = meta.hasColumn("project_id")
		meta.projectRequired = meta.hasProject && !meta.columns["project_id"].Nullable
		meta.hasVersion = meta.hasColumn("version")
		meta.unique = uniqueRules(table)
		index.tables[table.Name] = meta
	}
	index.writeRanks = buildWriteRanks(design, index.lockOrder)
	return index
}

// buildWriteRanks orders the tables a commit unit writes. Tables named by the
// declared lock order keep that order. A remaining schema table is placed
// immediately before the first lock-ordered table that references it, so an
// ownership parent always precedes its child inside one unit; a table nothing
// references is written last. This mirrors the PostgreSQL adapter so both
// adapters apply one unit in the same order.
func buildWriteRanks(design store.Schema, lockOrder map[string]int) map[string]float64 {
	ranks := make(map[string]float64, len(design.Tables))
	for _, table := range design.Tables {
		if position, ok := lockOrder[table.Name]; ok {
			ranks[table.Name] = float64(position)
		}
	}
	for _, table := range design.Tables {
		if _, ok := ranks[table.Name]; ok {
			continue
		}
		first := -1
		for _, candidate := range design.Tables {
			for _, key := range candidate.ForeignKeys {
				if key.References != table.Name {
					continue
				}
				position, ok := lockOrder[candidate.Name]
				if !ok {
					continue
				}
				if first < 0 || position < first {
					first = position
				}
			}
		}
		if first < 0 {
			ranks[table.Name] = float64(len(lockOrder)) + 0.5
			continue
		}
		ranks[table.Name] = float64(first) - 0.5
	}
	return ranks
}

// lookupTable returns the indexed metadata for one declared schema table.
func lookupTable(name string) (*tableMeta, bool) {
	meta, ok := schema.tables[name]
	return meta, ok
}

// writeRank returns the deterministic position one table is written at inside a
// commit unit.
func writeRank(name string) float64 {
	return schema.writeRanks[name]
}

// validIdentity reports whether value is a bounded ASCII identity payload, the
// same rule the neutral id package applies to every typed identity.
func validIdentity(value string) bool {
	if value == "" || len(value) > id.MaxIDLength {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for position := 0; position < len(value); position++ {
		character := value[position]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// faultInvalid builds the invalid_input class.
func faultInvalid(message string) error {
	return fault.New(fault.InvalidInput, message)
}

// faultForbidden builds the forbidden class for a cross-scope or
// scope-mismatch refusal.
func faultForbidden(message string) error {
	return fault.New(fault.Forbidden, message)
}

// faultConflict builds the version_conflict class.
func faultConflict(message string) error {
	return fault.New(fault.VersionConflict, message)
}

// faultNotFound builds the not_found class.
func faultNotFound(message string) error {
	return fault.New(fault.NotFound, message)
}

// faultInternal builds the internal_failure class.
func faultInternal(message string) error {
	return fault.New(fault.InternalFailure, message)
}
