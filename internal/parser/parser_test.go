package parser

import (
	"testing"

	"github.com/meonglotong/tsql/internal/types"
)

func mustParse(t *testing.T, src string) Statement {
	t.Helper()
	st, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", src, err)
	}
	return st
}

func TestParseCreate(t *testing.T) {
	st, ok := mustParse(t, "CREATE TABLE users (id INT PRIMARY KEY AUTO_INCREMENT, name TEXT NOT NULL, score FLOAT UNIQUE)").(*CreateTable)
	if !ok {
		t.Fatalf("not a CreateTable")
	}
	if st.Name != "users" || len(st.Cols) != 3 {
		t.Fatalf("got %+v", st)
	}
	id := st.Cols[0]
	if id.Name != "id" || id.Type != types.TypeInt || !id.Primary || !id.AutoInc {
		t.Errorf("col id: %+v", id)
	}
	name := st.Cols[1]
	if name.Name != "name" || name.Type != types.TypeText || !name.NotNull {
		t.Errorf("col name: %+v", name)
	}
	score := st.Cols[2]
	if score.Type != types.TypeFloat || !score.Unique {
		t.Errorf("col score: %+v", score)
	}
}

func TestParseReferences(t *testing.T) {
	st, ok := mustParse(t, "CREATE TABLE emp (id INT PRIMARY KEY, mgr INT REFERENCES users(id), dept INT REFERENCES depts)").(*CreateTable)
	if !ok {
		t.Fatalf("not a CreateTable")
	}
	mgr := st.Cols[1]
	if mgr.RefTable != "users" || mgr.RefCol != "id" {
		t.Errorf("col mgr: %+v", mgr)
	}
	dept := st.Cols[2]
	if dept.RefTable != "depts" || dept.RefCol != "" {
		t.Errorf("col dept: %+v", dept)
	}
}

func TestParseCreateAttributeOrder(t *testing.T) {
	st, _ := mustParse(t, "create table t (a text unique not null)").(*CreateTable)
	c := st.Cols[0]
	if !c.Unique || !c.NotNull || c.Type != types.TypeText {
		t.Errorf("attribute order not handled: %+v", c)
	}
}

func TestParseDrop(t *testing.T) {
	st, ok := mustParse(t, "DROP TABLE IF EXISTS users").(*DropTable)
	if !ok || st.Name != "users" || !st.IfExists {
		t.Fatalf("got %+v", st)
	}
	st2, ok := mustParse(t, "drop table users").(*DropTable)
	if !ok || st2.IfExists {
		t.Fatalf("got %+v", st2)
	}
}

func TestParseInsert(t *testing.T) {
	st, ok := mustParse(t, "INSERT INTO users (name, score) VALUES ('alfa', 1.5), ('beta', NULL)").(*Insert)
	if !ok {
		t.Fatal("not an Insert")
	}
	if st.Table != "users" || len(st.Cols) != 2 || len(st.Rows) != 2 {
		t.Fatalf("got %+v", st)
	}
	row0 := st.Rows[0]
	if len(row0) != 2 {
		t.Fatalf("row0 len = %d", len(row0))
	}
	lit, ok := row0[0].(*Lit)
	if !ok || lit.V.Kind != types.KindText || lit.V.S != "alfa" {
		t.Errorf("row0[0] = %+v", row0[0])
	}
	if row0[1].(*Lit).V.Kind != types.KindFloat || row0[1].(*Lit).V.F != 1.5 {
		t.Errorf("row0[1] = %+v", row0[1])
	}
	if st.Rows[1][1].(*Lit).V.Kind != types.KindNull {
		t.Errorf("row1[1] should be NULL: %+v", st.Rows[1][1])
	}
	// no column list
	st2, ok := mustParse(t, "insert into t values (1, 'x')").(*Insert)
	if !ok || len(st2.Cols) != 0 || len(st2.Rows[0]) != 2 {
		t.Fatalf("got %+v", st2)
	}
}

func TestParseSelect(t *testing.T) {
	st, ok := mustParse(t, "SELECT * FROM users WHERE score > 1 ORDER BY id DESC LIMIT 10").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if !st.Star || st.Tables[0].Table != "users" {
		t.Fatalf("got %+v", st)
	}
	if st.Where == nil {
		t.Fatal("where missing")
	}
	cmp, ok := st.Where.(*Cmp)
	if !ok || cmp.Op != ">" {
		t.Errorf("where = %+v", st.Where)
	}
	if len(st.Order) != 1 || !st.Order[0].Desc {
		t.Errorf("order = %+v", st.Order)
	}
	if ref, ok := st.Order[0].Expr.(*ColRef); !ok || ref.Name != "id" {
		t.Errorf("order expr = %+v", st.Order[0].Expr)
	}
	if st.Limit == nil || *st.Limit != 10 {
		t.Errorf("limit = %v", st.Limit)
	}
}

func TestParseSelectFieldsAndPrecedence(t *testing.T) {
	st, ok := mustParse(t, "SELECT name, score FROM users WHERE a = 1 AND b = 2 OR c = 3").(*Select)
	if !ok || st.Star || len(st.Fields) != 2 {
		t.Fatalf("got %+v", st)
	}
	// (a=1 AND b=2) OR c=3
	or, ok := st.Where.(*Or)
	if !ok {
		t.Fatalf("top-level should be OR: %+v", st.Where)
	}
	if _, ok := or.L.(*And); !ok {
		t.Fatalf("left of OR should be AND: %+v", or.L)
	}
	// 1 + 2 * 3 == 1 + (2*3)
	st2, _ := mustParse(t, "SELECT 1 + 2 * 3 FROM t").(*Select)
	bin, ok := st2.Fields[0].(*Binary)
	if !ok || bin.Op != "+" {
		t.Fatalf("top should be +: %+v", st2.Fields[0])
	}
	if _, ok := bin.R.(*Binary); !ok {
		t.Errorf("right of + should be *: %+v", bin.R)
	}
}

func TestParseUpdateDelete(t *testing.T) {
	st, ok := mustParse(t, "UPDATE users SET score = 9.5, name = 'x' WHERE id = 1").(*Update)
	if !ok {
		t.Fatal("not an Update")
	}
	if st.Table != "users" || len(st.Sets) != 2 || st.Sets[0].Col != "score" {
		t.Fatalf("got %+v", st)
	}
	if st.Where == nil {
		t.Error("where missing")
	}
	st2, ok := mustParse(t, "DELETE FROM users WHERE id = 1").(*Delete)
	if !ok || st2.Table != "users" || st2.Where == nil {
		t.Fatalf("got %+v", st2)
	}
	st3, ok := mustParse(t, "delete from users").(*Delete)
	if !ok || st3.Where != nil {
		t.Fatalf("got %+v", st3)
	}
}

func TestParseTxn(t *testing.T) {
	if _, ok := mustParse(t, "BEGIN").(*Begin); !ok {
		t.Error("BEGIN")
	}
	if _, ok := mustParse(t, "START TRANSACTION").(*Begin); !ok {
		t.Error("START TRANSACTION")
	}
	if _, ok := mustParse(t, "COMMIT").(*Commit); !ok {
		t.Error("COMMIT")
	}
	if _, ok := mustParse(t, "ROLLBACK").(*Rollback); !ok {
		t.Error("ROLLBACK")
	}
}

func TestParseLiteralKinds(t *testing.T) {
	st, ok := mustParse(t, "SELECT 42, -7, 3.5, 'a''b', true, null, now() FROM t").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	f := st.Fields
	if f[0].(*Lit).V.I != 42 {
		t.Errorf("f0: %+v", f[0])
	}
	// -7 parsed as 0 - 7
	neg, ok := f[1].(*Binary)
	if !ok || neg.Op != "-" {
		t.Fatalf("f1 should be 0-7: %+v", f[1])
	}
	if f[1].(*Binary).R.(*Lit).V.I != 7 {
		t.Errorf("f1: %+v", f[1])
	}
	if f[2].(*Lit).V.F != 3.5 {
		t.Errorf("f2: %+v", f[2])
	}
	if f[3].(*Lit).V.S != "a'b" {
		t.Errorf("f3: %+v", f[3])
	}
	if f[4].(*Lit).V.B != true {
		t.Errorf("f4: %+v", f[4])
	}
	if f[5].(*Lit).V.Kind != types.KindNull {
		t.Errorf("f5: %+v", f[5])
	}
	if f[6].(*Func).Name != "now" {
		t.Errorf("f6: %+v", f[6])
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"SELECTT * FROM t",
		"CREATE TABLE t (id int PRIMARY",
		"CREATE TABLE t (id int",      // missing close paren
		"CREATE TABLE t (id varchar)", // unknown type
		"INSERT INTO t VALUES (1,",    // dangling
		"SELECT * FROM",               // missing table
		"SELECT * FROM t WHERE =",     // dangling operator
		"SELECT * FROM t LIMIT 1.5",   // non-integer limit
		"SELECT * FROM t LIMIT x",     // non-literal limit
		"UPDATE t SET",                // missing assignment
		"TRUNCATE TABLE t",            // unsupported statement
		"SELECT * FROM t WHERE)",      // stray punct
		"SELECT 'unterminated FROM t", // unterminated string
		"SELECT * FROM t WHERE a = @", // bad character
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) should fail", src)
		}
	}
}

func TestParseJoin(t *testing.T) {
	st, ok := mustParse(t, "SELECT * FROM orders o JOIN users u ON u.id = o.user_id").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st.Tables) != 2 || len(st.On) != 1 {
		t.Fatalf("got %+v", st)
	}
	if st.Tables[0].Table != "orders" || st.Tables[0].Alias != "o" {
		t.Errorf("t0: %+v", st.Tables[0])
	}
	if st.Tables[1].Table != "users" || st.Tables[1].Alias != "u" {
		t.Errorf("t1: %+v", st.Tables[1])
	}
	cmp, ok := st.On[0].(*Cmp)
	if !ok || cmp.Op != "=" {
		t.Fatalf("on = %+v", st.On[0])
	}
	l, ok := cmp.L.(*ColRef)
	if !ok || l.Table != "u" || l.Name != "id" {
		t.Errorf("on.L = %+v", cmp.L)
	}
	r, ok := cmp.R.(*ColRef)
	if !ok || r.Table != "o" || r.Name != "user_id" {
		t.Errorf("on.R = %+v", cmp.R)
	}
}

func TestParseJoinChain(t *testing.T) {
	st, ok := mustParse(t, "SELECT a.x FROM a INNER JOIN b ON a.id = b.a_id JOIN c ON b.id = c.b_id").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st.Tables) != 3 || len(st.On) != 2 {
		t.Fatalf("got %+v", st)
	}
	for i, want := range []string{"a", "b", "c"} {
		if st.Tables[i].Table != want || st.Tables[i].Alias != want {
			t.Errorf("t%d = %+v", i, st.Tables[i])
		}
	}
}

func TestParseAliasAndStopwords(t *testing.T) {
	// explicit AS
	st, ok := mustParse(t, "SELECT * FROM users AS u WHERE u.id = 1").(*Select)
	if !ok || st.Tables[0].Alias != "u" {
		t.Fatalf("AS alias: %+v", st)
	}
	// no alias: "where" must not be eaten as an alias
	st2, ok := mustParse(t, "SELECT * FROM users WHERE id = 1").(*Select)
	if !ok || st2.Tables[0].Alias != "users" {
		t.Fatalf("stopword eaten as alias: %+v", st2)
	}
	// join table without explicit alias
	st3, ok := mustParse(t, "SELECT * FROM a JOIN b ON a.id = b.a_id").(*Select)
	if !ok || st3.Tables[1].Alias != "b" {
		t.Fatalf("implicit alias: %+v", st3)
	}
}

func TestParseGroupBy(t *testing.T) {
	st, ok := mustParse(t, "SELECT dept, count(*), sum(salary) FROM emp GROUP BY dept").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st.Group) != 1 {
		t.Fatalf("group = %+v", st.Group)
	}
	if ref, ok := st.Group[0].(*ColRef); !ok || ref.Name != "dept" {
		t.Errorf("group[0] = %+v", st.Group[0])
	}
	if len(st.Fields) != 3 {
		t.Fatalf("fields = %+v", st.Fields)
	}
	cnt, ok := st.Fields[1].(*Func)
	if !ok || cnt.Name != "count" || !cnt.Star || cnt.Arg != nil {
		t.Errorf("fields[1] = %+v", st.Fields[1])
	}
	sum, ok := st.Fields[2].(*Func)
	if !ok || sum.Name != "sum" || sum.Star {
		t.Fatalf("fields[2] = %+v", st.Fields[2])
	}
	if ref, ok := sum.Arg.(*ColRef); !ok || ref.Name != "salary" {
		t.Errorf("sum.Arg = %+v", sum.Arg)
	}
	// ORDER BY an aggregate
	st2, ok := mustParse(t, "SELECT dept, count(*) FROM emp GROUP BY dept ORDER BY count(*) DESC").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st2.Order) != 1 || !st2.Order[0].Desc {
		t.Fatalf("order = %+v", st2.Order)
	}
	if f, ok := st2.Order[0].Expr.(*Func); !ok || f.Name != "count" || !f.Star {
		t.Errorf("order expr = %+v", st2.Order[0].Expr)
	}
}

func TestParseFuncErrors(t *testing.T) {
	bad := []string{
		"SELECT * FROM t JOIN",
		"SELECT * FROM t JOIN t2", // missing ON
		"SELECT * FROM t GROUP",   // missing BY
		"SELECT * FROM t INNER",   // dangling INNER
		"SELECT count(*)",         // missing FROM
		"SELECT sum() FROM t",     // empty parens is fine to parse; reject sum with no arg at exec
	}
	for _, src := range bad[:5] {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) should fail", src)
		}
	}
	// sum() parses (executor rejects it)
	if st, err := Parse("SELECT sum() FROM t"); err != nil {
		t.Errorf("Parse(sum()) should not fail at parse time: %v", err)
	} else if sel, ok := st.(*Select); !ok || sel.Fields[0].(*Func).Arg != nil {
		t.Errorf("sum() shape wrong: %+v", st)
	}
}

// --- v2: LEFT JOIN, HAVING, subqueries, IN ---------------------------------

func TestParseLeftJoin(t *testing.T) {
	st, ok := mustParse(t, "SELECT a.x FROM a LEFT JOIN b ON a.id = b.aid").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st.Tables) != 2 || len(st.JoinKinds) != 1 || st.JoinKinds[0] != "left" {
		t.Fatalf("got Tables=%v JoinKinds=%v", st.Tables, st.JoinKinds)
	}
	st2, ok := mustParse(t, "SELECT a.x FROM a LEFT OUTER JOIN b ON a.id = b.aid").(*Select)
	if !ok || st2.JoinKinds[0] != "left" {
		t.Fatalf("LEFT OUTER JOIN: %+v", st2.JoinKinds)
	}
	st3, ok := mustParse(t, "SELECT a.x FROM a INNER JOIN b ON a.id = b.aid").(*Select)
	if !ok || st3.JoinKinds[0] != "inner" {
		t.Fatalf("INNER JOIN: %+v", st3.JoinKinds)
	}
	st4, ok := mustParse(t, "SELECT a.x FROM a JOIN b ON a.id = b.aid JOIN c ON c.id = b.id").(*Select)
	if !ok || len(st4.JoinKinds) != 2 || st4.JoinKinds[0] != "inner" || st4.JoinKinds[1] != "inner" {
		t.Fatalf("multi join: %+v", st4.JoinKinds)
	}
}

func TestParseHaving(t *testing.T) {
	st, ok := mustParse(t, "SELECT dept, count(*) FROM t GROUP BY dept HAVING count(*) > 1 ORDER BY dept LIMIT 5").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if st.Having == nil {
		t.Fatal("Having is nil")
	}
	cmp, ok := st.Having.(*Cmp)
	if !ok || cmp.Op != ">" {
		t.Fatalf("Having = %+v", st.Having)
	}
	fn, ok := cmp.L.(*Func)
	if !ok || fn.Name != "count" || !fn.Star {
		t.Fatalf("Having.L = %+v", cmp.L)
	}
	// HAVING without GROUP BY must also parse
	if _, err := Parse("SELECT count(*) FROM t HAVING count(*) > 3"); err != nil {
		t.Errorf("Parse: %v", err)
	}
}

func TestParseFromSubquery(t *testing.T) {
	st, ok := mustParse(t, "SELECT name FROM (SELECT id, name FROM users WHERE score > 1) s").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	tr := st.Tables[0]
	if tr.Sub == nil || tr.Alias != "s" || tr.Table != "" {
		t.Fatalf("derived table: %+v", tr)
	}
	inner := tr.Sub.Tables[0].Table
	if inner != "users" {
		t.Fatalf("inner table: %q", inner)
	}
	// derived table may be joined
	st2, ok := mustParse(t, "SELECT s.name FROM (SELECT id, name FROM users) s JOIN depts d ON d.id = s.id").(*Select)
	if !ok || len(st2.Tables) != 2 || st2.Tables[0].Sub == nil {
		t.Fatalf("join with derived: %+v", st2.Tables)
	}
	// a derived table must have an alias
	if _, err := Parse("SELECT * FROM (SELECT id FROM users)"); err == nil {
		t.Error("derived table without alias should fail")
	}
}

func TestParseIn(t *testing.T) {
	st, ok := mustParse(t, "SELECT id FROM t WHERE id IN (1, 2, 3)").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	in, ok := st.Where.(*InList)
	if !ok || in.Not || len(in.List) != 3 {
		t.Fatalf("Where = %+v", st.Where)
	}
	st2, ok := mustParse(t, "SELECT id FROM t WHERE id NOT IN (1)").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	in2, ok := st2.Where.(*InList)
	if !ok || !in2.Not || len(in2.List) != 1 {
		t.Fatalf("Where = %+v", st2.Where)
	}
	st3, ok := mustParse(t, "SELECT id FROM t WHERE id IN (SELECT id FROM x)").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	in3, ok := st3.Where.(*InSub)
	if !ok || in3.Not || in3.Sub == nil {
		t.Fatalf("Where = %+v", st3.Where)
	}
	st4, ok := mustParse(t, "SELECT id FROM t WHERE id NOT IN (SELECT id FROM x)").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	in4, ok := st4.Where.(*InSub)
	if !ok || !in4.Not {
		t.Fatalf("Where = %+v", st4.Where)
	}
}

func TestParseSelectAlias(t *testing.T) {
	// AS alias
	st, ok := mustParse(t, "SELECT name AS nama FROM users").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st.Fields) != 1 || len(st.FieldAliases) != 1 || st.FieldAliases[0] != "nama" {
		t.Fatalf("FieldAliases = %+v", st.FieldAliases)
	}

	// bare aliases (no AS)
	st2, ok := mustParse(t, "SELECT name nm, dept d FROM users").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st2.FieldAliases) != 2 || st2.FieldAliases[0] != "nm" || st2.FieldAliases[1] != "d" {
		t.Fatalf("FieldAliases = %+v", st2.FieldAliases)
	}

	// no alias -> empty string
	st3, ok := mustParse(t, "SELECT name FROM users").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if len(st3.FieldAliases) != 1 || st3.FieldAliases[0] != "" {
		t.Fatalf("FieldAliases = %+v", st3.FieldAliases)
	}

	// alias on aggregate
	st4, ok := mustParse(t, "SELECT count(*) total FROM users").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if st4.FieldAliases[0] != "total" {
		t.Fatalf("FieldAliases = %+v", st4.FieldAliases)
	}

	// clause keyword must not be eaten as alias
	st5, ok := mustParse(t, "SELECT id FROM t ORDER BY id").(*Select)
	if !ok {
		t.Fatal("not a Select")
	}
	if st5.FieldAliases[0] != "" {
		t.Fatalf("'from' eaten as alias: %+v", st5.FieldAliases)
	}
}
