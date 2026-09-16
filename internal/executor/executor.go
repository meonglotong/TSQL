// Package executor runs parsed TSQL statements against a storage engine.
//
// v1/v2 semantics:
//   - SELECT supports inner joins (left-deep), table aliases, qualified
//     column references, WHERE, GROUP BY with aggregates (count/sum/avg/
//     min/max), ORDER BY and LIMIT.
//   - UPDATE / DELETE take a single table.
//   - UNIQUE columns are backed by a secondary index in the storage engine
//     (equality lookup shortcut + duplicate rejection).
package executor

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meonglotong/tsql/internal/parser"
	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/storage"
	"github.com/meonglotong/tsql/internal/types"
)

// SQLError carries a SQLSTATE-like code for the protocol layer.
type SQLError struct {
	Code string
	Msg  string
}

func (e *SQLError) Error() string { return e.Msg }

func sqlErr(code, msg string) error { return &SQLError{Code: code, Msg: msg} }

// Result is the outcome of one statement.
type Result struct {
	Columns  []string
	Rows     [][]types.Value
	Affected int
}

// Exec runs one statement. eng is used for DDL (autocommit); acc is the
// read/write view for DML and SELECT — the engine itself for autocommit, or
// an explicit transaction for BEGIN/COMMIT/ROLLBACK sessions.
func Exec(eng *storage.Engine, acc storage.Accessor, stmt parser.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *parser.CreateTable:
		return execCreate(eng, s)
	case *parser.DropTable:
		return execDrop(eng, s)
	case *parser.Insert:
		return execInsert(acc, s)
	case *parser.Select:
		return execSelect(acc, s)
	case *parser.Update:
		return execUpdate(acc, s)
	case *parser.Delete:
		return execDelete(acc, s)
	default:
		return nil, sqlErr("0A000", "unsupported statement")
	}
}

// --- helpers ----------------------------------------------------------------

func tableDef(acc storage.Accessor, name string) (*schema.TableDef, error) {
	for _, d := range acc.Catalog() {
		if d.Name == name {
			return &d, nil
		}
	}
	return nil, sqlErr("42P01", fmt.Sprintf("relation \"%s\" does not exist", name))
}

func colIndexMap(def *schema.TableDef) map[string]int {
	m := map[string]int{}
	for i, c := range def.Cols {
		m[c.Name] = i
	}
	return m
}

func pkIndex(def *schema.TableDef) (int, bool) {
	for i, c := range def.Cols {
		if c.Primary {
			return i, true
		}
	}
	return 0, false
}

// coerce applies a column type check, with a convenience: text literals are
// parsed as timestamps when the target column is a timestamp.
func coerce(col schema.Column, v types.Value) (types.Value, error) {
	if v.Kind == types.KindText && col.Type == types.TypeTimestamp {
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
			if tm, err := time.ParseInLocation(layout, v.S, time.UTC); err == nil {
				v = types.Time(tm)
				break
			}
		}
	}
	return col.Type.Check(v)
}

// --- join scope --------------------------------------------------------------

// joinTable is one table (or alias) in a FROM clause.
type joinTable struct {
	def     *schema.TableDef
	alias   string
	basePos int // first column position in the joined row
	colIdx  map[string]int
}

// joinScope resolves column references across joined tables.
type joinScope struct {
	tables []joinTable
}

func singleTableScope(def *schema.TableDef) *joinScope {
	return &joinScope{tables: []joinTable{{
		def: def, alias: def.Name, basePos: 0, colIdx: colIndexMap(def),
	}}}
}

// sub returns a scope over the first n tables (the left-deep join prefix).
func (sc *joinScope) sub(n int) *joinScope {
	if n >= len(sc.tables) {
		return sc
	}
	cp := make([]joinTable, n)
	copy(cp, sc.tables[:n])
	return &joinScope{tables: cp}
}

// resolve maps a column reference to its position in the joined row.
// Unqualified names that exist in more than one table are ambiguous.
func (sc *joinScope) resolve(ref *parser.ColRef) (int, error) {
	if ref.Table != "" {
		for i := range sc.tables {
			if sc.tables[i].alias == ref.Table {
				if ci, ok := sc.tables[i].colIdx[ref.Name]; ok {
					return sc.tables[i].basePos + ci, nil
				}
				return -1, sqlErr("42703", fmt.Sprintf("column \"%s\" of table \"%s\" does not exist", ref.Name, ref.Table))
			}
		}
		return -1, sqlErr("42P01", fmt.Sprintf("missing FROM-clause entry for table \"%s\"", ref.Table))
	}
	var pos []int
	for i := range sc.tables {
		if ci, ok := sc.tables[i].colIdx[ref.Name]; ok {
			pos = append(pos, sc.tables[i].basePos+ci)
		}
	}
	if len(pos) == 0 {
		return -1, sqlErr("42703", fmt.Sprintf("column \"%s\" does not exist", ref.Name))
	}
	if len(pos) > 1 {
		return -1, sqlErr("42703", fmt.Sprintf("column name \"%s\" is ambiguous", ref.Name))
	}
	return pos[0], nil
}

// headerAt returns the result-column header for a joined-row position:
// plain column name for single-table queries, "alias.col" otherwise.
func (sc *joinScope) headerAt(pos int) string {
	for i := range sc.tables {
		t := &sc.tables[i]
		if pos >= t.basePos && pos < t.basePos+len(t.def.Cols) {
			ci := pos - t.basePos
			if len(sc.tables) == 1 {
				return t.def.Cols[ci].Name
			}
			return t.alias + "." + t.def.Cols[ci].Name
		}
	}
	return "?"
}

// validateExpr resolves every column reference in e against sc at plan time,
// so unknown or ambiguous columns fail before any row is scanned.
func validateExpr(sc *joinScope, e parser.Expr) error {
	switch x := e.(type) {
	case *parser.Lit:
		return nil
	case *parser.ColRef:
		_, err := sc.resolve(x)
		return err
	case *parser.Func:
		switch {
		case x.Name == "now":
			return nil
		case parser.IsAggregateFunc(x.Name):
			return sqlErr("42883", fmt.Sprintf("aggregate function %s() is not allowed in WHERE or SET clauses", x.Name))
		default:
			return sqlErr("42883", fmt.Sprintf("function %s() is not supported in v1", x.Name))
		}
	case *parser.Cmp:
		if err := validateExpr(sc, x.L); err != nil {
			return err
		}
		return validateExpr(sc, x.R)
	case *parser.IsNull:
		return validateExpr(sc, x.X)
	case *parser.And:
		if err := validateExpr(sc, x.L); err != nil {
			return err
		}
		return validateExpr(sc, x.R)
	case *parser.Or:
		if err := validateExpr(sc, x.L); err != nil {
			return err
		}
		return validateExpr(sc, x.R)
	case *parser.Not:
		return validateExpr(sc, x.X)
	case *parser.Binary:
		if err := validateExpr(sc, x.L); err != nil {
			return err
		}
		return validateExpr(sc, x.R)
	}
	return nil
}

// --- DDL --------------------------------------------------------------------

func execCreate(eng *storage.Engine, s *parser.CreateTable) (*Result, error) {
	cols := make([]schema.Column, len(s.Cols))
	for i, c := range s.Cols {
		cols[i] = schema.Column{
			Name: c.Name, Type: c.Type, Primary: c.Primary,
			NotNull: c.NotNull, AutoInc: c.AutoInc, Unique: c.Unique,
		}
		if c.RefTable != "" {
			if err := validateFK(eng, s.Name, &cols[i], c.RefTable, c.RefCol); err != nil {
				return nil, err
			}
		}
	}
	if err := eng.CreateTable(s.Name, cols); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, sqlErr("42P07", fmt.Sprintf("relation \"%s\" already exists", s.Name))
		}
		return nil, sqlErr("42601", err.Error())
	}
	return &Result{}, nil
}

func execDrop(eng *storage.Engine, s *parser.DropTable) (*Result, error) {
	if err := eng.DropTable(s.Name); err != nil {
		if errors.Is(err, storage.ErrNoTable) {
			if s.IfExists {
				return &Result{}, nil
			}
			return nil, sqlErr("42P01", fmt.Sprintf("relation \"%s\" does not exist", s.Name))
		}
		return nil, err
	}
	return &Result{}, nil
}

// --- Foreign keys (RESTRICT, v1: single column -> parent PK) -----------------

// validateFK resolves a REFERENCES clause and records it in col. v1 rules:
// the parent table must already exist, the target is its primary key
// (explicit or default), and the column types must match exactly.
func validateFK(eng *storage.Engine, thisTable string, col *schema.Column, refTable, refCol string) error {
	if refTable == thisTable {
		return sqlErr("42P01", fmt.Sprintf("foreign key references unknown table %q (self references need the table to exist first in v1)", refTable))
	}
	var pdef *schema.TableDef
	for _, d := range eng.Catalog() {
		if d.Name == refTable {
			dd := d
			pdef = &dd
			break
		}
	}
	if pdef == nil {
		return sqlErr("42P01", fmt.Sprintf("foreign key references unknown table %q", refTable))
	}
	target := refCol
	if target == "" {
		for _, pc := range pdef.Cols {
			if pc.Primary {
				target = pc.Name
				break
			}
		}
		if target == "" {
			return sqlErr("23552", fmt.Sprintf("table %q has no primary key to reference", refTable))
		}
	}
	var pt *schema.Column
	for _, pc := range pdef.Cols {
		if pc.Name == target {
			pcc := pc
			pt = &pcc
			break
		}
	}
	if pt == nil {
		return sqlErr("23552", fmt.Sprintf("column %q does not exist in table %q", target, refTable))
	}
	if !pt.Primary {
		return sqlErr("0A000", fmt.Sprintf("foreign key to non-primary-key column %q.%q is not supported in v1", refTable, target))
	}
	if pt.Type != col.Type {
		return sqlErr("42804", fmt.Sprintf("foreign key column %q type %s does not match referenced column %q type %s", col.Name, col.Type, pt.Name, pt.Type))
	}
	col.FKTable = refTable
	col.FKCol = target
	return nil
}

// checkFK verifies that every non-NULL FK value in row points to an existing
// parent row in acc's (possibly buffered) view. v1: FK targets are always the
// parent's primary key, so the lookup is a direct get by row id.
func checkFK(acc storage.Accessor, def *schema.TableDef, row []types.Value) error {
	for ci, c := range def.Cols {
		if c.FKTable == "" {
			continue
		}
		v := row[ci]
		if v.IsNull() {
			continue // NULL FKs are allowed
		}
		if v.Kind != types.KindInt {
			return sqlErr("23503", fmt.Sprintf("foreign key value in column %q of table %q is not an integer", c.Name, def.Name))
		}
		if _, ok := acc.Get(c.FKTable, v.I); !ok {
			return sqlErr("23503", fmt.Sprintf("insert or update on table %q violates foreign key constraint (no row %d in %q)", def.Name, v.I, c.FKTable))
		}
	}
	return nil
}

// fkReferenced reports whether any row in any table (in acc's view) references
// the given parent primary key value. It enforces RESTRICT on parent PK
// deletes and PK moves. skipTable/skipRowID excludes the row being
// deleted/moved itself (self-referencing tables).
func fkReferenced(acc storage.Accessor, parent string, pkValue int64, skipTable string, skipRowID int64) error {
	for _, d := range acc.Catalog() {
		for ci, c := range d.Cols {
			if c.FKTable != parent {
				continue
			}
			ids, rows, err := acc.AllRows(d.Name)
			if err != nil {
				continue
			}
			for i, r := range rows {
				if d.Name == skipTable && ids[i] == skipRowID {
					continue
				}
				if len(r) > ci && r[ci].Kind == types.KindInt && r[ci].I == pkValue {
					return sqlErr("23503", fmt.Sprintf("update or delete on table %q violates foreign key constraint (row referenced by %q)", parent, d.Name))
				}
			}
		}
	}
	return nil
}

// --- DML --------------------------------------------------------------------

func execInsert(acc storage.Accessor, s *parser.Insert) (*Result, error) {
	def, err := tableDef(acc, s.Table)
	if err != nil {
		return nil, err
	}
	ncols := len(def.Cols)
	var idx []int
	if len(s.Cols) == 0 {
		for i := range def.Cols {
			idx = append(idx, i)
		}
	} else {
		seen := map[int]bool{}
		for _, cn := range s.Cols {
			found := -1
			for i, c := range def.Cols {
				if c.Name == cn {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, sqlErr("42703", fmt.Sprintf("column \"%s\" of relation \"%s\" does not exist", cn, s.Table))
			}
			if seen[found] {
				return nil, sqlErr("42703", fmt.Sprintf("column \"%s\" specified more than once", cn))
			}
			seen[found] = true
			idx = append(idx, found)
		}
	}
	pkIdx, hasPK := pkIndex(def)
	for ri, rowExprs := range s.Rows {
		if len(rowExprs) != len(idx) {
			return nil, sqlErr("21000", fmt.Sprintf("INSERT has %d expressions but %d columns were specified (row %d)", len(rowExprs), len(idx), ri+1))
		}
		row := make([]types.Value, ncols)
		for ci, targetIdx := range idx {
			v, err := evalInsertValue(rowExprs[ci])
			if err != nil {
				return nil, err
			}
			row[targetIdx], err = coerce(def.Cols[targetIdx], v)
			if err != nil {
				return nil, sqlErr("22P02", fmt.Sprintf("column \"%s\": %v", def.Cols[targetIdx].Name, err))
			}
		}
		// primary key handling
		var rowID int64
		auto := false
		if hasPK {
			pv := row[pkIdx]
			switch {
			case pv.IsNull():
				if def.Cols[pkIdx].AutoInc {
					auto = true
				} else {
					return nil, sqlErr("23502", fmt.Sprintf("null value in column \"%s\" of relation \"%s\" violates not-null constraint", def.Cols[pkIdx].Name, s.Table))
				}
			case pv.Kind == types.KindInt:
				rowID = pv.I
			default:
				return nil, sqlErr("22P02", fmt.Sprintf("column \"%s\": primary key value must be integer", def.Cols[pkIdx].Name))
			}
		}
		// NOT NULL for non-pk columns
		for i, c := range def.Cols {
			if c.Primary {
				continue
			}
			if c.NotNull && row[i].IsNull() {
				return nil, sqlErr("23502", fmt.Sprintf("null value in column \"%s\" of relation \"%s\" violates not-null constraint", c.Name, s.Table))
			}
		}
		if err := checkFK(acc, def, row); err != nil {
			return nil, err
		}
		var insertErr error
		if auto {
			_, insertErr = acc.PutAuto(s.Table, pkIdx, row)
		} else {
			_, insertErr = acc.Put(s.Table, rowID, row)
		}
		if insertErr != nil {
			switch {
			case errors.Is(insertErr, storage.ErrExists):
				return nil, sqlErr("23505", fmt.Sprintf("duplicate key value violates unique constraint on \"%s\" (id %d)", s.Table, rowID))
			case errors.Is(insertErr, storage.ErrUnique):
				return nil, sqlErr("23505", fmt.Sprintf("duplicate key value violates unique constraint on \"%s\"", s.Table))
			}
			return nil, insertErr
		}
	}
	return &Result{Affected: len(s.Rows)}, nil
}

// evalInsertValue: VALUES clauses take literals (and NOW()).
func evalInsertValue(e parser.Expr) (types.Value, error) {
	switch x := e.(type) {
	case *parser.Lit:
		return x.V, nil
	case *parser.Func:
		if x.Name == "now" {
			return types.Time(time.Now().UTC()), nil
		}
		return types.Null(), sqlErr("42883", fmt.Sprintf("function %s() is not supported in v1", x.Name))
	default:
		return types.Null(), sqlErr("0A000", "only literals are allowed in VALUES (v1)")
	}
}

// --- SELECT -----------------------------------------------------------------

type fieldKind int

const (
	fieldCol  fieldKind = iota // joined-row column (also the GROUP BY value)
	fieldAgg                   // aggregate over a group's rows
	fieldNow                   // now()
	fieldExpr                  // general row expression
)

type fieldPlan struct {
	kind fieldKind
	pos  int // joined-row position (fieldCol / fieldAgg argument)
	agg  *parser.Func
	expr parser.Expr
}

func (sc *joinScope) planFields(s *parser.Select, grouping bool, groupPosSet map[int]bool) ([]fieldPlan, []string, error) {
	var plans []fieldPlan
	var headers []string
	add := func(p fieldPlan, h string) {
		plans = append(plans, p)
		headers = append(headers, h)
	}
	if s.Star {
		if grouping {
			return nil, nil, sqlErr("42803", "SELECT * is not allowed with GROUP BY or aggregate functions")
		}
		for i := range sc.tables {
			t := &sc.tables[i]
			for j := range t.def.Cols {
				add(fieldPlan{kind: fieldCol, pos: t.basePos + j}, sc.headerAt(t.basePos+j))
			}
		}
		return plans, headers, nil
	}
	for _, f := range s.Fields {
		switch x := f.(type) {
		case *parser.ColRef:
			pos, err := sc.resolve(x)
			if err != nil {
				return nil, nil, err
			}
			if grouping && !groupPosSet[pos] {
				return nil, nil, sqlErr("42803", fmt.Sprintf("column %q must appear in the GROUP BY clause or be used in an aggregate function", x.Name))
			}
			add(fieldPlan{kind: fieldCol, pos: pos}, sc.headerAt(pos))
		case *parser.Func:
			switch {
			case x.Name == "now":
				add(fieldPlan{kind: fieldNow}, "now()")
			case parser.IsAggregateFunc(x.Name):
				if !grouping {
					return nil, nil, sqlErr("42803", "aggregate functions require GROUP BY in v1")
				}
				pos, err := validateAggregate(x, sc)
				if err != nil {
					return nil, nil, err
				}
				add(fieldPlan{kind: fieldAgg, pos: pos, agg: x}, aggHeader(x))
			default:
				return nil, nil, sqlErr("42883", fmt.Sprintf("function %s() is not supported in v1", x.Name))
			}
		default:
			if grouping {
				return nil, nil, sqlErr("42803", "non-aggregate expressions must appear in the GROUP BY clause")
			}
			if err := validateExpr(sc, f); err != nil {
				return nil, nil, err
			}
			add(fieldPlan{kind: fieldExpr, expr: f}, "expr")
		}
	}
	return plans, headers, nil
}

// validateAggregate checks the shape of an aggregate call and returns the
// joined-row position of its column argument (-1 for count(*)).
func validateAggregate(f *parser.Func, sc *joinScope) (int, error) {
	if f.Star {
		if f.Name != "count" {
			return -1, sqlErr("42883", fmt.Sprintf("%s(*) is not supported", f.Name))
		}
		return -1, nil
	}
	if f.Arg == nil {
		return -1, sqlErr("42883", fmt.Sprintf("%s() requires an argument", f.Name))
	}
	ref, ok := f.Arg.(*parser.ColRef)
	if !ok {
		return -1, sqlErr("42883", "aggregate argument must be a column reference in v1")
	}
	return sc.resolve(ref)
}

func aggHeader(f *parser.Func) string {
	if f.Star {
		return f.Name + "(*)"
	}
	if ref, ok := f.Arg.(*parser.ColRef); ok {
		if ref.Table != "" {
			return f.Name + "(" + ref.Table + "." + ref.Name + ")"
		}
		return f.Name + "(" + ref.Name + ")"
	}
	return f.Name + "(?)"
}

// uniqueEquality reports whether where is exactly `col = literal` on a UNIQUE
// column of def. lit is the literal coerced to the column type, or nil when
// the literal is NULL (which matches no rows).
func uniqueEquality(where parser.Expr, alias string, def *schema.TableDef) (int, *types.Value, bool) {
	cmp, ok := where.(*parser.Cmp)
	if !ok || cmp.Op != "=" {
		return 0, nil, false
	}
	var ref *parser.ColRef
	var lit *parser.Lit
	switch l := cmp.L.(type) {
	case *parser.ColRef:
		ref = l
		lit, ok = cmp.R.(*parser.Lit)
		if !ok {
			return 0, nil, false
		}
	case *parser.Lit:
		lit = l
		ref, ok = cmp.R.(*parser.ColRef)
		if !ok {
			return 0, nil, false
		}
	default:
		return 0, nil, false
	}
	if ref.Table != "" && ref.Table != alias {
		return 0, nil, false
	}
	for i, c := range def.Cols {
		if c.Name == ref.Name && c.Unique {
			cv, err := c.Type.Check(lit.V)
			if err != nil {
				return 0, nil, false
			}
			l := cv
			return i, &l, true
		}
	}
	return 0, nil, false
}

// collectJoined loads all FROM tables, runs the left-deep nested-loop join,
// and applies the WHERE filter.
func collectJoined(acc storage.Accessor, sc *joinScope, s *parser.Select) ([][]types.Value, error) {
	tableRows := make([][][]types.Value, len(sc.tables))
	for i := range sc.tables {
		_, rows, err := acc.AllRows(sc.tables[i].def.Name)
		if err != nil {
			return nil, err
		}
		tableRows[i] = rows
	}
	joined := tableRows[0]
	for i := 1; i < len(sc.tables); i++ {
		partial := sc.sub(i + 1)
		on := s.On[i-1]
		next := make([][]types.Value, 0, len(joined))
		for _, lrow := range joined {
			for _, rrow := range tableRows[i] {
				full := make([]types.Value, 0, len(lrow)+len(rrow))
				full = append(full, lrow...)
				full = append(full, rrow...)
				b, isNull, err := evalBool(partial, on, full)
				if err != nil {
					return nil, err
				}
				if b && !isNull {
					next = append(next, full)
				}
			}
		}
		joined = next
	}
	var filtered [][]types.Value
	for _, row := range joined {
		if s.Where != nil {
			b, isNull, err := evalBool(sc, s.Where, row)
			if err != nil {
				return nil, err
			}
			if !(b && !isNull) {
				continue
			}
		}
		filtered = append(filtered, row)
	}
	return filtered, nil
}

func execSelect(acc storage.Accessor, s *parser.Select) (*Result, error) {
	// Build the join scope.
	var jtables []joinTable
	seenAlias := map[string]bool{}
	basePos := 0
	for _, tr := range s.Tables {
		def, err := tableDef(acc, tr.Table)
		if err != nil {
			return nil, err
		}
		if seenAlias[tr.Alias] {
			return nil, sqlErr("42703", fmt.Sprintf("table alias %q specified more than once", tr.Alias))
		}
		seenAlias[tr.Alias] = true
		jtables = append(jtables, joinTable{def: def, alias: tr.Alias, basePos: basePos, colIdx: colIndexMap(def)})
		basePos += len(def.Cols)
	}
	sc := &joinScope{tables: jtables}

	// Validate join conditions against the left-deep prefix including the
	// table being joined (an ON may reference both sides of the join).
	for i := range s.On {
		if err := validateExpr(sc.sub(i+2), s.On[i]); err != nil {
			return nil, err
		}
	}
	if s.Where != nil {
		if err := validateExpr(sc, s.Where); err != nil {
			return nil, err
		}
	}

	// GROUP BY columns.
	var groupPos []int
	for _, g := range s.Group {
		ref, ok := g.(*parser.ColRef)
		if !ok {
			return nil, sqlErr("42601", "GROUP BY accepts column references in v1")
		}
		pos, err := sc.resolve(ref)
		if err != nil {
			return nil, err
		}
		groupPos = append(groupPos, pos)
	}
	groupPosSet := map[int]bool{}
	for _, p := range groupPos {
		groupPosSet[p] = true
	}
	hasAgg := false
	for _, f := range s.Fields {
		if fn, ok := f.(*parser.Func); ok && parser.IsAggregateFunc(fn.Name) {
			hasAgg = true
			break
		}
	}
	grouping := len(groupPos) > 0 || hasAgg

	plans, headers, err := sc.planFields(s, grouping, groupPosSet)
	if err != nil {
		return nil, err
	}

	// ORDER BY plan.
	var orderPlans []fieldPlan
	if grouping {
		for _, ot := range s.Order {
			switch x := ot.Expr.(type) {
			case *parser.ColRef:
				pos, err := sc.resolve(x)
				if err != nil {
					return nil, err
				}
				if !groupPosSet[pos] {
					return nil, sqlErr("42803", fmt.Sprintf("column %q must appear in the GROUP BY clause or be used in an aggregate function", x.Name))
				}
				orderPlans = append(orderPlans, fieldPlan{kind: fieldCol, pos: pos})
			case *parser.Func:
				if x.Name == "now" {
					orderPlans = append(orderPlans, fieldPlan{kind: fieldNow})
				} else if parser.IsAggregateFunc(x.Name) {
					pos, err := validateAggregate(x, sc)
					if err != nil {
						return nil, err
					}
					orderPlans = append(orderPlans, fieldPlan{kind: fieldAgg, pos: pos, agg: x})
				} else {
					return nil, sqlErr("42883", fmt.Sprintf("function %s() is not supported in v1", x.Name))
				}
			case *parser.Lit:
				orderPlans = append(orderPlans, fieldPlan{kind: fieldExpr, expr: x})
			default:
				return nil, sqlErr("42803", "ORDER BY must reference a GROUP BY column or an aggregate in v1")
			}
		}
	} else {
		for _, ot := range s.Order {
			if err := validateExpr(sc, ot.Expr); err != nil {
				return nil, err
			}
			if ref, ok := ot.Expr.(*parser.ColRef); ok {
				pos, err := sc.resolve(ref)
				if err != nil {
					return nil, err
				}
				orderPlans = append(orderPlans, fieldPlan{kind: fieldCol, pos: pos})
			} else {
				orderPlans = append(orderPlans, fieldPlan{kind: fieldExpr, expr: ot.Expr})
			}
		}
	}

	// Candidate rows: UNIQUE-index shortcut for single-table equality lookups,
	// nested-loop join otherwise.
	var filtered [][]types.Value
	if len(jtables) == 1 {
		if ci, lit, ok := uniqueEquality(s.Where, jtables[0].alias, jtables[0].def); ok {
			// Exactly one row can match; the index says which.
			if lit != nil {
				if id, found := acc.UniqueGet(jtables[0].def.Name, ci, *lit); found {
					if row, ok2 := acc.Get(jtables[0].def.Name, id); ok2 {
						b, isNull, err := evalBool(sc, s.Where, row)
						if err != nil {
							return nil, err
						}
						if b && !isNull {
							filtered = append(filtered, row)
						}
					}
				}
			}
		} else {
			filtered, err = collectJoined(acc, sc, s)
			if err != nil {
				return nil, err
			}
		}
	} else {
		filtered, err = collectJoined(acc, sc, s)
		if err != nil {
			return nil, err
		}
	}
	if !grouping {
		if len(orderPlans) > 0 {
			type keyedRow struct {
				row  []types.Value
				keys []types.Value
			}
			krs := make([]keyedRow, 0, len(filtered))
			for _, row := range filtered {
				keys := make([]types.Value, len(orderPlans))
				for oi, p := range orderPlans {
					var v types.Value
					var err error
					switch p.kind {
					case fieldCol:
						v = row[p.pos]
					case fieldNow:
						v = types.Time(time.Now().UTC())
					default:
						v, err = evalValue(sc, p.expr, row)
					}
					if err != nil {
						return nil, err
					}
					keys[oi] = v
				}
				krs = append(krs, keyedRow{row: row, keys: keys})
			}
			sort.SliceStable(krs, func(a, b int) bool {
				ka, kb := krs[a].keys, krs[b].keys
				for k := range ka {
					c := ka[k].Compare(kb[k])
					if c != 0 {
						if s.Order[k].Desc {
							return c > 0
						}
						return c < 0
					}
				}
				return false
			})
			out := make([][]types.Value, len(krs))
			for i, kr := range krs {
				out[i] = kr.row
			}
			filtered = out
		}
		if s.Limit != nil && int64(len(filtered)) > *s.Limit {
			filtered = filtered[:*s.Limit]
		}
		res := &Result{Columns: headers}
		for _, row := range filtered {
			out := make([]types.Value, len(plans))
			for fi, p := range plans {
				switch p.kind {
				case fieldCol:
					out[fi] = row[p.pos]
				case fieldNow:
					out[fi] = types.Time(time.Now().UTC())
				default:
					v, err := evalValue(sc, p.expr, row)
					if err != nil {
						return nil, err
					}
					out[fi] = v
				}
			}
			res.Rows = append(res.Rows, out)
		}
		return res, nil
	}

	// --- GROUP BY path ---
	nowVal := types.Time(time.Now().UTC())
	type grp struct {
		repr []types.Value // first row of the group
		rows [][]types.Value
	}
	groups := make([]grp, 0)
	gi := make(map[string]int)
	for _, row := range filtered {
		var sb strings.Builder
		for _, p := range groupPos {
			sb.WriteString(valKeyPart(row[p]))
			sb.WriteByte(0)
		}
		key := sb.String()
		idx, ok := gi[key]
		if !ok {
			idx = len(groups)
			gi[key] = idx
			repr := make([]types.Value, len(row))
			copy(repr, row)
			groups = append(groups, grp{repr: repr})
		}
		groups[idx].rows = append(groups[idx].rows, row)
	}
	// Aggregate without GROUP BY over an empty input is one empty group
	// (count -> 0, sum/avg/min/max -> NULL), matching PostgreSQL.
	if len(groupPos) == 0 && len(groups) == 0 {
		groups = append(groups, grp{})
	}
	if len(orderPlans) > 0 {
		keys := make([][]types.Value, len(groups))
		for i := range groups {
			keys[i] = make([]types.Value, len(orderPlans))
			for oi, p := range orderPlans {
				var v types.Value
				var err error
				switch p.kind {
				case fieldCol:
					v = groups[i].repr[p.pos]
				case fieldAgg:
					v, err = evalAggregate(p.agg, groups[i].rows, p.pos)
				case fieldNow:
					v = nowVal
				default:
					v, err = evalValue(sc, p.expr, groups[i].repr)
				}
				if err != nil {
					return nil, err
				}
				keys[i][oi] = v
			}
		}
		ord := make([]int, len(groups))
		for i := range ord {
			ord[i] = i
		}
		sort.SliceStable(ord, func(a, b int) bool {
			ka, kb := keys[ord[a]], keys[ord[b]]
			for k := range ka {
				c := ka[k].Compare(kb[k])
				if c != 0 {
					if s.Order[k].Desc {
						return c > 0
					}
					return c < 0
				}
			}
			return false
		})
		sorted := make([]grp, len(groups))
		for i, o := range ord {
			sorted[i] = groups[o]
		}
		groups = sorted
	}
	if s.Limit != nil && int64(len(groups)) > *s.Limit {
		groups = groups[:*s.Limit]
	}
	res := &Result{Columns: headers}
	for _, g := range groups {
		out := make([]types.Value, len(plans))
		for fi, p := range plans {
			switch p.kind {
			case fieldCol:
				out[fi] = g.repr[p.pos]
			case fieldAgg:
				v, err := evalAggregate(p.agg, g.rows, p.pos)
				if err != nil {
					return nil, err
				}
				out[fi] = v
			case fieldNow:
				out[fi] = nowVal
			default:
				v, err := evalValue(sc, p.expr, g.repr)
				if err != nil {
					return nil, err
				}
				out[fi] = v
			}
		}
		res.Rows = append(res.Rows, out)
	}
	return res, nil
}

// --- UPDATE / DELETE ----------------------------------------------------------

type candidate struct {
	id  int64
	row []types.Value
}

// collectCandidates returns the rows matching where (nil where = all rows),
// using the UNIQUE index when where is a plain `uniquecol = literal`.
func collectCandidates(acc storage.Accessor, name string, def *schema.TableDef, sc *joinScope, where parser.Expr) ([]candidate, error) {
	if where != nil {
		if ci, lit, ok := uniqueEquality(where, name, def); ok {
			var cands []candidate
			if lit != nil {
				if id, found := acc.UniqueGet(name, ci, *lit); found {
					if row, ok2 := acc.Get(name, id); ok2 {
						b, isNull, err := evalBool(sc, where, row)
						if err != nil {
							return nil, err
						}
						if b && !isNull {
							cands = append(cands, candidate{id, row})
						}
					}
				}
			}
			return cands, nil
		}
	}
	ids, rows, err := acc.AllRows(name)
	if err != nil {
		return nil, sqlErr("42P01", fmt.Sprintf("relation \"%s\" does not exist", name))
	}
	var cands []candidate
	for i, row := range rows {
		if where != nil {
			b, isNull, err := evalBool(sc, where, row)
			if err != nil {
				return nil, err
			}
			if !(b && !isNull) {
				continue
			}
		}
		cands = append(cands, candidate{ids[i], row})
	}
	return cands, nil
}

func execUpdate(acc storage.Accessor, s *parser.Update) (*Result, error) {
	def, err := tableDef(acc, s.Table)
	if err != nil {
		return nil, err
	}
	sc := singleTableScope(def)
	type setCol struct {
		idx  int
		expr parser.Expr
	}
	sets := make([]setCol, len(s.Sets))
	for i, a := range s.Sets {
		idx := -1
		for j, c := range def.Cols {
			if c.Name == a.Col {
				idx = j
				break
			}
		}
		if idx < 0 {
			return nil, sqlErr("42703", fmt.Sprintf("column \"%s\" does not exist", a.Col))
		}
		sets[i] = setCol{idx: idx, expr: a.Expr}
	}
	for i := range s.Sets {
		if err := validateExpr(sc, s.Sets[i].Expr); err != nil {
			return nil, err
		}
	}
	if s.Where != nil {
		if err := validateExpr(sc, s.Where); err != nil {
			return nil, err
		}
	}
	pkIdx, hasPK := pkIndex(def)

	cands, err := collectCandidates(acc, s.Table, def, sc, s.Where)
	if err != nil {
		return nil, err
	}
	affected := 0
	for _, c := range cands {
		newRow := make([]types.Value, len(c.row))
		copy(newRow, c.row)
		for _, scSet := range sets {
			v, err := evalValue(sc, scSet.expr, c.row) // evaluate against the original row
			if err != nil {
				return nil, err
			}
			cv, err := coerce(def.Cols[scSet.idx], v)
			if err != nil {
				return nil, sqlErr("22P02", fmt.Sprintf("column \"%s\": %v", def.Cols[scSet.idx].Name, err))
			}
			newRow[scSet.idx] = cv
		}
		if err := checkFK(acc, def, newRow); err != nil {
			return nil, err
		}
		rowID := c.id
		if hasPK && newRow[pkIdx].Kind == types.KindInt &&
			c.row[pkIdx].Kind == types.KindInt && newRow[pkIdx].I != c.row[pkIdx].I {
			// PK changed: move the row (RESTRICT: children block the move)
			if err := fkReferenced(acc, s.Table, rowID, s.Table, rowID); err != nil {
				return nil, err
			}
			if err := acc.Delete(s.Table, rowID); err != nil {
				return nil, err
			}
			if _, err := acc.Put(s.Table, newRow[pkIdx].I, newRow); err != nil {
				if errors.Is(err, storage.ErrExists) {
					return nil, sqlErr("23505", fmt.Sprintf("duplicate key value violates unique constraint on \"%s\" (id %d)", s.Table, newRow[pkIdx].I))
				}
				if errors.Is(err, storage.ErrUnique) {
					return nil, sqlErr("23505", fmt.Sprintf("duplicate key value violates unique constraint on \"%s\"", s.Table))
				}
				return nil, err
			}
		} else if err := acc.Update(s.Table, rowID, newRow); err != nil {
			if errors.Is(err, storage.ErrUnique) {
				return nil, sqlErr("23505", fmt.Sprintf("duplicate key value violates unique constraint on \"%s\"", s.Table))
			}
			return nil, err
		}
		affected++
	}
	return &Result{Affected: affected}, nil
}

func execDelete(acc storage.Accessor, s *parser.Delete) (*Result, error) {
	def, err := tableDef(acc, s.Table)
	if err != nil {
		return nil, err
	}
	sc := singleTableScope(def)
	if s.Where != nil {
		if err := validateExpr(sc, s.Where); err != nil {
			return nil, err
		}
	}
	cands, err := collectCandidates(acc, s.Table, def, sc, s.Where)
	if err != nil {
		return nil, err
	}
	affected := 0
	for _, c := range cands {
		// RESTRICT: children block the delete of a referenced parent pk
		if err := fkReferenced(acc, s.Table, c.id, s.Table, c.id); err != nil {
			return nil, err
		}
		if err := acc.Delete(s.Table, c.id); err != nil {
			return nil, err
		}
		affected++
	}
	return &Result{Affected: affected}, nil
}

// --- Expression evaluation ---------------------------------------------------

// evalValue evaluates an expression against one (joined) row.
func evalValue(sc *joinScope, e parser.Expr, row []types.Value) (types.Value, error) {
	switch x := e.(type) {
	case *parser.Lit:
		return x.V, nil
	case *parser.ColRef:
		i, err := sc.resolve(x)
		if err != nil {
			return types.Null(), err
		}
		return row[i], nil
	case *parser.Func:
		if x.Name == "now" {
			return types.Time(time.Now().UTC()), nil
		}
		if parser.IsAggregateFunc(x.Name) {
			return types.Null(), sqlErr("42883", fmt.Sprintf("aggregate function %s() is not allowed in this context", x.Name))
		}
		return types.Null(), sqlErr("42883", fmt.Sprintf("function %s() is not supported in v1", x.Name))
	case *parser.Binary:
		l, err := evalValue(sc, x.L, row)
		if err != nil {
			return types.Null(), err
		}
		r, err := evalValue(sc, x.R, row)
		if err != nil {
			return types.Null(), err
		}
		if l.IsNull() || r.IsNull() {
			return types.Null(), nil
		}
		return arithmetic(x.Op, l, r)
	default:
		return types.Null(), sqlErr("0A000", "unsupported expression")
	}
}

func arithmetic(op string, l, r types.Value) (types.Value, error) {
	num := func(v types.Value) bool {
		return v.Kind == types.KindInt || v.Kind == types.KindFloat
	}
	if !num(l) || !num(r) {
		return types.Null(), sqlErr("42883", fmt.Sprintf("operator %q is not defined for %s and %s", op, l.Kind, r.Kind))
	}
	bothInt := l.Kind == types.KindInt && r.Kind == types.KindInt
	a, b := float64(l.I), float64(r.I)
	if l.Kind == types.KindFloat {
		a = l.F
	}
	if r.Kind == types.KindFloat {
		b = r.F
	}
	if (op == "/" || op == "%") && b == 0 {
		return types.Null(), sqlErr("22012", "division by zero")
	}
	switch op {
	case "+":
		if bothInt {
			return types.Int(l.I + r.I), nil
		}
		return types.Float(a + b), nil
	case "-":
		if bothInt {
			return types.Int(l.I - r.I), nil
		}
		return types.Float(a - b), nil
	case "*":
		if bothInt {
			return types.Int(l.I * r.I), nil
		}
		return types.Float(a * b), nil
	case "/":
		if bothInt {
			return types.Int(l.I / r.I), nil
		}
		return types.Float(a / b), nil
	case "%":
		if bothInt {
			return types.Int(l.I % r.I), nil
		}
		return types.Float(math.Mod(a, b)), nil
	}
	return types.Null(), sqlErr("0A000", "unknown operator "+op)
}

// evalBool evaluates a boolean expression with SQL three-valued logic and
// returns (value, isNull).
func evalBool(sc *joinScope, e parser.Expr, row []types.Value) (bool, bool, error) {
	switch x := e.(type) {
	case *parser.Cmp:
		l, err := evalValue(sc, x.L, row)
		if err != nil {
			return false, false, err
		}
		r, err := evalValue(sc, x.R, row)
		if err != nil {
			return false, false, err
		}
		if l.IsNull() || r.IsNull() {
			return false, true, nil
		}
		c := l.Compare(r)
		switch x.Op {
		case "=":
			return c == 0, false, nil
		case "<>":
			return c != 0, false, nil
		case "<":
			return c < 0, false, nil
		case "<=":
			return c <= 0, false, nil
		case ">":
			return c > 0, false, nil
		case ">=":
			return c >= 0, false, nil
		}
		return false, false, sqlErr("0A000", "unknown comparison operator "+x.Op)
	case *parser.IsNull:
		v, err := evalValue(sc, x.X, row)
		if err != nil {
			return false, false, err
		}
		if x.Not {
			return !v.IsNull(), false, nil
		}
		return v.IsNull(), false, nil
	case *parser.And:
		lt, ln, err := evalBool(sc, x.L, row)
		if err != nil {
			return false, false, err
		}
		rt, rn, err := evalBool(sc, x.R, row)
		if err != nil {
			return false, false, err
		}
		if (ln && !rt) || (rn && !lt) {
			return false, true, nil
		}
		return lt && rt, false, nil
	case *parser.Or:
		lt, ln, err := evalBool(sc, x.L, row)
		if err != nil {
			return false, false, err
		}
		rt, rn, err := evalBool(sc, x.R, row)
		if err != nil {
			return false, false, err
		}
		if (ln && !rt) || (rn && !lt) {
			return false, true, nil
		}
		return lt || rt, false, nil
	case *parser.Not:
		v, n, err := evalBool(sc, x.X, row)
		if err != nil {
			return false, false, err
		}
		if n {
			return false, true, nil
		}
		return !v, false, nil
	case *parser.Lit:
		switch x.V.Kind {
		case types.KindBool:
			return x.V.B, false, nil
		case types.KindNull:
			return false, true, nil
		}
		return false, false, sqlErr("42883", "expression is not boolean")
	case *parser.Binary:
		v, err := evalValue(sc, x, row)
		if err != nil {
			return false, false, err
		}
		if v.IsNull() {
			return false, true, nil
		}
		switch v.Kind {
		case types.KindBool:
			return v.B, false, nil
		case types.KindInt:
			return v.I != 0, false, nil
		case types.KindFloat:
			return v.F != 0, false, nil
		}
		return false, false, sqlErr("42883", "expression is not boolean")
	default:
		return false, false, sqlErr("0A000", "unsupported expression in boolean context")
	}
}

// --- Aggregates ----------------------------------------------------------------

// evalAggregate computes one aggregate over the rows of one group.
func evalAggregate(f *parser.Func, rows [][]types.Value, argPos int) (types.Value, error) {
	switch f.Name {
	case "count":
		if f.Star {
			return types.Int(int64(len(rows))), nil
		}
		n := int64(0)
		for _, r := range rows {
			if !r[argPos].IsNull() {
				n++
			}
		}
		return types.Int(n), nil
	case "sum":
		sumI := int64(0)
		sumF := 0.0
		any := false
		allInt := true
		for _, r := range rows {
			v := r[argPos]
			if v.IsNull() {
				continue
			}
			any = true
			switch v.Kind {
			case types.KindInt:
				sumI += v.I
			case types.KindFloat:
				sumF += v.F
				allInt = false
			default:
				return types.Null(), sqlErr("42883", "sum() requires a numeric column")
			}
		}
		if !any {
			return types.Null(), nil
		}
		if allInt {
			return types.Int(sumI), nil
		}
		return types.Float(float64(sumI) + sumF), nil
	case "avg":
		sum := 0.0
		n := int64(0)
		for _, r := range rows {
			v := r[argPos]
			if v.IsNull() {
				continue
			}
			switch v.Kind {
			case types.KindInt:
				sum += float64(v.I)
				n++
			case types.KindFloat:
				sum += v.F
				n++
			default:
				return types.Null(), sqlErr("42883", "avg() requires a numeric column")
			}
		}
		if n == 0 {
			return types.Null(), nil
		}
		return types.Float(sum / float64(n)), nil
	case "min", "max":
		var best types.Value
		found := false
		for _, r := range rows {
			v := r[argPos]
			if v.IsNull() {
				continue
			}
			if !found {
				best = v
				found = true
				continue
			}
			c := v.Compare(best)
			if (f.Name == "min" && c < 0) || (f.Name == "max" && c > 0) {
				best = v
			}
		}
		if !found {
			return types.Null(), nil
		}
		return best, nil
	}
	return types.Null(), sqlErr("42883", fmt.Sprintf("unknown aggregate function %s()", f.Name))
}

// valKeyPart is a collision-free encoding of one value, used for group keys.
func valKeyPart(v types.Value) string {
	switch v.Kind {
	case types.KindInt:
		return "i" + strconv.FormatInt(v.I, 10)
	case types.KindFloat:
		return "f" + strconv.FormatFloat(v.F, 'g', -1, 64)
	case types.KindText:
		return "s" + strconv.Quote(v.S)
	case types.KindBool:
		if v.B {
			return "b1"
		}
		return "b0"
	case types.KindTime:
		return "t" + strconv.FormatInt(v.T.UnixNano(), 10)
	default:
		return "n"
	}
}
