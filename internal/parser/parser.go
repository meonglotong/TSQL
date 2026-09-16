// Package parser turns TSQL text into AST nodes.
//
// Grammar (keywords are case-insensitive; identifiers are lowercased):
//
//	create   := CREATE TABLE ident '(' colDef (',' colDef)* ')'
//	colDef   := ident typeName { PRIMARY KEY | NOT NULL | AUTO_INCREMENT | UNIQUE }*
//	drop     := DROP TABLE [IF EXISTS] ident
//	insert   := INSERT INTO ident ['(' ident (',' ident)* ')'] VALUES tuple (',' tuple)*
//	select   := SELECT selectList FROM tableRef {[INNER] JOIN tableRef ON expr}
//	           [WHERE expr] [GROUP BY exprList] [ORDER BY orderList] [LIMIT int]
//	tableRef := ident [AS] [ident]
//	update   := UPDATE ident SET ident '=' expr (',' ident '=' expr)* [WHERE expr]
//	delete   := DELETE FROM ident [WHERE expr]
//	txn      := BEGIN | START TRANSACTION | COMMIT | ROLLBACK
//
//	Columns may be qualified: a.col. Functions: count/sum/avg/min/max (aggregate),
//	now(). Expr precedence: OR < AND < NOT < comparisons < arithmetic.
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/meonglotong/tsql/internal/types"
)

// --- AST ------------------------------------------------------------------

// Statement is a parsed TSQL statement.
type Statement interface {
	stmt()
}

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*Insert) stmt()      {}
func (*Select) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}

// CreateTable is CREATE TABLE.
type CreateTable struct {
	Name string
	Cols []ColumnDef
}

// ColumnDef is one column inside CREATE TABLE.
type ColumnDef struct {
	Name    string
	Type    types.ColumnType
	Primary bool
	NotNull bool
	AutoInc bool
	Unique  bool
	// RefTable/RefCol describe REFERENCES: the parent table and the
	// referenced column (empty = the parent's primary key).
	RefTable string
	RefCol   string
}

// DropTable is DROP TABLE.
type DropTable struct {
	Name     string
	IfExists bool
}

// Insert is INSERT INTO ... VALUES ...
type Insert struct {
	Table string
	Cols  []string // empty = all table columns
	Rows  [][]Expr
}

// TableRef is one table in a FROM clause, with its alias (defaults to the
// table name).
type TableRef struct {
	Table string
	Alias string
}

// Select is SELECT.
type Select struct {
	Tables []TableRef // FROM + joined tables, in order
	On     []Expr     // ON expr for Tables[i+1], aligned: On[i] joins Tables[i+1]
	Star   bool
	Fields []Expr // ignored when Star
	Where  Expr
	Group  []Expr // v1: column references only
	Order  []OrderTerm
	Limit  *int64
}

// OrderTerm is one ORDER BY item (column ref or aggregate in v1).
type OrderTerm struct {
	Expr Expr
	Desc bool
}

// Update is UPDATE ... SET ...
type Update struct {
	Table string
	Sets  []Assignment
	Where Expr
}

// Assignment is one col = expr inside SET.
type Assignment struct {
	Col  string
	Expr Expr
}

// Delete is DELETE FROM.
type Delete struct {
	Table string
	Where Expr
}

// Begin / Commit / Rollback are transaction statements.
type Begin struct{}
type Commit struct{}
type Rollback struct{}

// --- Expressions ----------------------------------------------------------

// Expr is a parsed expression node.
type Expr interface {
	expr()
}

func (*Lit) expr()    {}
func (*ColRef) expr() {}
func (*Func) expr()   {}
func (*Cmp) expr()    {}
func (*IsNull) expr() {}
func (*And) expr()    {}
func (*Or) expr()     {}
func (*Not) expr()    {}
func (*Binary) expr() {}

// Lit is a literal value (number, string, bool, NULL).
type Lit struct{ V types.Value }

// ColRef references a column, optionally qualified with a table alias
// (Table empty = unqualified).
type ColRef struct {
	Table string
	Name  string
}

// Func is a function call. v1: now(), and the aggregates
// count/sum/avg/min/max (Star for count(*), Arg for count(col) etc.).
type Func struct {
	Name string
	Star bool // count(*)
	Arg  Expr // count(col) — nil for now() and star form
}

// Cmp is a comparison: =, <>, <, <=, >, >=
type Cmp struct {
	Op   string
	L, R Expr
}

// IsNull is `expr IS [NOT] NULL`.
type IsNull struct {
	X   Expr
	Not bool
}

// And / Or / Not are logical operators.
type And struct{ L, R Expr }
type Or struct{ L, R Expr }
type Not struct{ X Expr }

// Binary is arithmetic: +, -, *, /, %
type Binary struct {
	Op   string
	L, R Expr
}

// IsAggregateFunc reports whether the name is a v1 aggregate function.
func IsAggregateFunc(name string) bool {
	switch name {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

// Parse parses one TSQL statement. A trailing ';' is allowed.
func Parse(src string) (Statement, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.atPunct(";") {
		p.next()
	}
	if p.peek().kind != tokEOF {
		return nil, p.errf("unexpected input %s after statement", describe(p.peek()))
	}
	return stmt, nil
}

// --- Lexer ----------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNum
	tokStr
	tokPunct
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func describe(t token) string {
	if t.kind == tokEOF {
		return "end of input"
	}
	return fmt.Sprintf("%q", t.text)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

const punctChars = "(),=<>+-*%/;."

func lex(src string) ([]token, error) {
	var toks []token
	pos := 0
	for pos < len(src) {
		c := src[pos]
		start := pos
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			pos++
		case c == '\'':
			pos++
			var sb strings.Builder
			closed := false
			for pos < len(src) {
				d := src[pos]
				if d == '\'' {
					if pos+1 < len(src) && src[pos+1] == '\'' {
						sb.WriteByte('\'')
						pos += 2
						continue
					}
					pos++
					closed = true
					break
				}
				sb.WriteByte(d)
				pos++
			}
			if !closed {
				return nil, fmt.Errorf("syntax error at position %d: unterminated string literal", start)
			}
			toks = append(toks, token{kind: tokStr, text: sb.String(), pos: start})
		case c >= '0' && c <= '9':
			for pos < len(src) && src[pos] >= '0' && src[pos] <= '9' {
				pos++
			}
			if pos < len(src) && src[pos] == '.' {
				pos++
				for pos < len(src) && src[pos] >= '0' && src[pos] <= '9' {
					pos++
				}
			}
			toks = append(toks, token{kind: tokNum, text: src[start:pos], pos: start})
		case isIdentStart(c):
			for pos < len(src) && isIdentChar(src[pos]) {
				pos++
			}
			toks = append(toks, token{kind: tokIdent, text: strings.ToLower(src[start:pos]), pos: start})
		case strings.IndexByte(punctChars, c) >= 0:
			if pos+1 < len(src) {
				if two := src[pos : pos+2]; two == "<=" || two == ">=" || two == "<>" {
					toks = append(toks, token{kind: tokPunct, text: two, pos: start})
					pos += 2
					break
				}
			}
			toks = append(toks, token{kind: tokPunct, text: string(c), pos: start})
			pos++
		default:
			return nil, fmt.Errorf("syntax error at position %d: unexpected character %q", start, c)
		}
	}
	toks = append(toks, token{kind: tokEOF, pos: len(src)})
	return toks, nil
}

// --- Parser ----------------------------------------------------------------

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) errf(format string, a ...any) error {
	pos := p.peek().pos
	return fmt.Errorf("syntax error at position %d: "+format, append([]any{pos}, a...)...)
}

func (p *parser) atPunct(s string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.text == s
}

func (p *parser) expectPunct(s string) error {
	t := p.peek()
	if t.kind == tokPunct && t.text == s {
		p.next()
		return nil
	}
	return p.errf("expected %q, got %s", s, describe(t))
}

func (p *parser) atKw(kw string) bool {
	t := p.peek()
	return t.kind == tokIdent && t.text == kw
}

func (p *parser) expectKw(kw string) error {
	t := p.peek()
	if t.kind == tokIdent && t.text == kw {
		p.next()
		return nil
	}
	return p.errf("expected %q, got %s", kw, describe(t))
}

// takeKw consumes the keyword if present and reports whether it did.
func (p *parser) takeKw(kw string) bool {
	if p.atKw(kw) {
		p.next()
		return true
	}
	return false
}

func (p *parser) identOrErr(what string) (string, error) {
	t := p.peek()
	if t.kind != tokIdent {
		return "", p.errf("expected %s, got %s", what, describe(t))
	}
	p.next()
	return t.text, nil
}

// refStopWords are clause keywords that must not be consumed as a table alias.
var refStopWords = map[string]bool{
	"where": true, "group": true, "order": true, "limit": true,
	"join": true, "inner": true, "left": true, "right": true,
	"outer": true, "cross": true, "on": true, "having": true,
}

func (p *parser) parseStatement() (Statement, error) {
	switch {
	case p.atKw("create"):
		return p.parseCreate()
	case p.atKw("drop"):
		return p.parseDrop()
	case p.atKw("insert"):
		return p.parseInsert()
	case p.atKw("select"):
		return p.parseSelect()
	case p.atKw("update"):
		return p.parseUpdate()
	case p.atKw("delete"):
		return p.parseDelete()
	case p.atKw("begin"):
		p.next()
		return &Begin{}, nil
	case p.atKw("start"):
		p.next()
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		return &Begin{}, nil
	case p.atKw("commit"):
		p.next()
		return &Commit{}, nil
	case p.atKw("rollback"):
		p.next()
		return &Rollback{}, nil
	case p.peek().kind == tokEOF:
		return nil, p.errf("empty statement")
	default:
		return nil, p.errf("unsupported statement (got %s)", describe(p.peek()))
	}
}

func (p *parser) parseCreate() (*CreateTable, error) {
	p.next() // create
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	name, err := p.identOrErr("table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	st := &CreateTable{Name: name}
	for {
		cd, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		st.Cols = append(st.Cols, cd)
		if p.atPunct(",") {
			p.next()
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return st, nil
}

func (p *parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.identOrErr("column name")
	if err != nil {
		return ColumnDef{}, err
	}
	tn, err := p.identOrErr("column type")
	if err != nil {
		return ColumnDef{}, err
	}
	if !types.ValidTypes[types.ColumnType(tn)] {
		return ColumnDef{}, p.errf("unknown column type %q", tn)
	}
	cd := ColumnDef{Name: name, Type: types.ColumnType(tn)}
	for {
		switch {
		case p.takeKw("primary"):
			if err := p.expectKw("key"); err != nil {
				return cd, err
			}
			cd.Primary = true
		case p.takeKw("not"):
			if err := p.expectKw("null"); err != nil {
				return cd, err
			}
			cd.NotNull = true
		case p.takeKw("auto_increment"):
			cd.AutoInc = true
		case p.takeKw("unique"):
			cd.Unique = true
		case p.takeKw("references"):
			ref, err := p.identOrErr("referenced table name")
			if err != nil {
				return cd, err
			}
			cd.RefTable = ref
			if p.atPunct("(") {
				p.next()
				c, err := p.identOrErr("referenced column name")
				if err != nil {
					return cd, err
				}
				cd.RefCol = c
				if err := p.expectPunct(")"); err != nil {
					return cd, err
				}
			}
		default:
			return cd, nil
		}
	}
}

func (p *parser) parseDrop() (*DropTable, error) {
	p.next() // drop
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	st := &DropTable{}
	if p.takeKw("if") {
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		st.IfExists = true
	}
	name, err := p.identOrErr("table name")
	if err != nil {
		return nil, err
	}
	st.Name = name
	return st, nil
}

func (p *parser) parseInsert() (*Insert, error) {
	p.next() // insert
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	name, err := p.identOrErr("table name")
	if err != nil {
		return nil, err
	}
	st := &Insert{Table: name}
	if p.atPunct("(") {
		p.next()
		for {
			c, err := p.identOrErr("column name")
			if err != nil {
				return nil, err
			}
			st.Cols = append(st.Cols, c)
			if p.atPunct(",") {
				p.next()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var row []Expr
		if !p.atPunct(")") {
			for {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				row = append(row, e)
				if p.atPunct(",") {
					p.next()
					continue
				}
				break
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		st.Rows = append(st.Rows, row)
		if p.atPunct(",") {
			p.next()
			continue
		}
		break
	}
	return st, nil
}

// parseTableRef parses `ident [AS] [ident]` — a table with an optional alias.
func (p *parser) parseTableRef() (TableRef, error) {
	name, err := p.identOrErr("table name")
	if err != nil {
		return TableRef{}, err
	}
	ref := TableRef{Table: name, Alias: name}
	if p.takeKw("as") {
		a, err := p.identOrErr("alias")
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = a
	} else if p.peek().kind == tokIdent && !refStopWords[p.peek().text] {
		a, err := p.identOrErr("alias")
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = a
	}
	return ref, nil
}

func (p *parser) parseSelect() (*Select, error) {
	p.next() // select
	st := &Select{}
	if p.atPunct("*") {
		p.next()
		st.Star = true
	} else {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			st.Fields = append(st.Fields, e)
			if p.atPunct(",") {
				p.next()
				continue
			}
			break
		}
	}
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	ref, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	st.Tables = append(st.Tables, ref)
	for {
		if p.atKw("join") {
			p.next() // join
		} else if p.atKw("inner") {
			save := p.i
			p.next()
			if !p.atKw("join") {
				p.i = save // dangling INNER: restore, top-level will error
				break
			}
			p.next() // join
		} else {
			break
		}
		ref, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("on"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Tables = append(st.Tables, ref)
		st.On = append(st.On, on)
	}
	if p.atKw("where") {
		p.next()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	if p.atKw("group") {
		p.next()
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			st.Group = append(st.Group, e)
			if p.atPunct(",") {
				p.next()
				continue
			}
			break
		}
	}
	if p.atKw("order") {
		p.next()
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ot := OrderTerm{Expr: e}
			switch {
			case p.takeKw("desc"):
				ot.Desc = true
			case p.takeKw("asc"):
			}
			st.Order = append(st.Order, ot)
			if p.atPunct(",") {
				p.next()
				continue
			}
			break
		}
	}
	if p.atKw("limit") {
		p.next()
		t := p.peek()
		if t.kind != tokNum || strings.Contains(t.text, ".") {
			return nil, p.errf("LIMIT requires a non-negative integer literal")
		}
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil || n < 0 {
			return nil, p.errf("LIMIT requires a non-negative integer literal")
		}
		p.next()
		st.Limit = &n
	}
	return st, nil
}

func (p *parser) parseUpdate() (*Update, error) {
	p.next() // update
	name, err := p.identOrErr("table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	st := &Update{Table: name}
	for {
		c, err := p.identOrErr("column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("="); err != nil {
			return nil, err
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Sets = append(st.Sets, Assignment{Col: c, Expr: e})
		if p.atPunct(",") {
			p.next()
			continue
		}
		break
	}
	if p.atKw("where") {
		p.next()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	return st, nil
}

func (p *parser) parseDelete() (*Delete, error) {
	p.next() // delete
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	name, err := p.identOrErr("table name")
	if err != nil {
		return nil, err
	}
	st := &Delete{Table: name}
	if p.atKw("where") {
		p.next()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	return st, nil
}

// --- Expression parser (precedence: OR < AND < NOT < cmp < add < mul) -----

func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.atKw("or") {
		p.next()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &Or{L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.atKw("and") {
		p.next()
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &And{L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.atKw("not") {
		p.next()
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Not{X: x}, nil
	}
	return p.parseCmp()
}

func (p *parser) parseCmp() (Expr, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tokPunct && (t.text == "=" || t.text == "<>" || t.text == "<" ||
		t.text == "<=" || t.text == ">" || t.text == ">=") {
		p.next()
		r, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &Cmp{Op: t.text, L: l, R: r}, nil
	}
	if p.atKw("is") {
		p.next()
		not := p.takeKw("not")
		if err := p.expectKw("null"); err != nil {
			return nil, err
		}
		return &IsNull{X: l, Not: not}, nil
	}
	return l, nil
}

func (p *parser) parseAdd() (Expr, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == tokPunct && (t.text == "+" || t.text == "-") {
			p.next()
			r, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			l = &Binary{Op: t.text, L: l, R: r}
			continue
		}
		break
	}
	return l, nil
}

func (p *parser) parseMul() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == tokPunct && (t.text == "*" || t.text == "/" || t.text == "%") {
			p.next()
			r, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			l = &Binary{Op: t.text, L: l, R: r}
			continue
		}
		break
	}
	return l, nil
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if t.kind == tokPunct && t.text == "-" {
		p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Binary{Op: "-", L: &Lit{V: types.Int(0)}, R: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokNum:
		p.next()
		if strings.Contains(t.text, ".") {
			f, err := strconv.ParseFloat(t.text, 64)
			if err != nil {
				return nil, p.errf("bad number %q", t.text)
			}
			return &Lit{V: types.Float(f)}, nil
		}
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, p.errf("bad number %q", t.text)
		}
		return &Lit{V: types.Int(n)}, nil
	case tokStr:
		p.next()
		return &Lit{V: types.Text(t.text)}, nil
	case tokIdent:
		switch t.text {
		case "null":
			p.next()
			return &Lit{V: types.Null()}, nil
		case "true":
			p.next()
			return &Lit{V: types.Bool(true)}, nil
		case "false":
			p.next()
			return &Lit{V: types.Bool(false)}, nil
		}
		p.next()
		// qualified column reference: a.col
		if p.atPunct(".") {
			p.next()
			cn, err := p.identOrErr("column name")
			if err != nil {
				return nil, err
			}
			return &ColRef{Table: t.text, Name: cn}, nil
		}
		// function call: name('*' | expr)
		if p.atPunct("(") {
			p.next()
			f := &Func{Name: t.text}
			if p.atPunct("*") {
				p.next()
				f.Star = true
			} else if !p.atPunct(")") {
				arg, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				f.Arg = arg
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return f, nil
		}
		return &ColRef{Name: t.text}, nil
	case tokPunct:
		if t.text == "(" {
			p.next()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
		return nil, p.errf("unexpected %s", describe(t))
	default:
		return nil, p.errf("unexpected end of input")
	}
}
