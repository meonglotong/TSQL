// Package schema holds the TSQL catalog types: table and column definitions.
package schema

import (
	"fmt"
	"regexp"

	"github.com/meonglotong/tsql/internal/types"
)

// Column is one column definition (v1).
// FK fields were added in Fase 3 (gob ignores unknown fields, so old
// snapshots stay compatible).
type Column struct {
	Name    string
	Type    types.ColumnType
	NotNull bool
	Primary bool
	AutoInc bool
	Unique  bool
	// FKTable/FKCol describe a REFERENCES clause: the parent table and the
	// referenced column (always the parent's primary key in v1). Empty
	// FKTable = no foreign key.
	FKTable string
	FKCol   string
}

// TableDef is the full definition of one table.
type TableDef struct {
	Name string
	Cols []Column
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate checks a table definition against v1 rules. It normalizes the
// primary key column type in place (v1 PKs must be int/bigint).
func (td *TableDef) Validate() error {
	if !identRe.MatchString(td.Name) {
		return fmt.Errorf("invalid table name %q", td.Name)
	}
	if len(td.Cols) == 0 {
		return fmt.Errorf("table %s needs at least one column", td.Name)
	}
	seen := map[string]bool{}
	pkCount := 0
	for i := range td.Cols {
		c := &td.Cols[i]
		if !identRe.MatchString(c.Name) {
			return fmt.Errorf("invalid column name %q in table %s", c.Name, td.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate column %q in table %s", c.Name, td.Name)
		}
		seen[c.Name] = true
		if !types.ValidTypes[c.Type] {
			return fmt.Errorf("column %s: unknown type %q", c.Name, c.Type)
		}
		if c.Primary {
			pkCount++
			c.Type = normalizePKType(c.Type)
		}
	}
	if pkCount > 1 {
		return fmt.Errorf("table %s: v1 supports at most one primary key column", td.Name)
	}
	return nil
}

// normalizePKType: v1 primary keys must be integer (index + auto-increment
// rely on an integer key).
func normalizePKType(ct types.ColumnType) types.ColumnType {
	if ct == types.TypeInt || ct == types.TypeBigint {
		return ct
	}
	return types.TypeInt
}
