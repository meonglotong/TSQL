package main

import "testing"

func TestClassifyLine(t *testing.T) {
	cases := []struct {
		line, kind, arg string
	}{
		// quit forms
		{"\\q", "quit", ""},
		{"quit", "quit", ""},
		{"EXIT", "quit", ""},
		// meta commands (backslash AND forward slash aliases)
		{"\\dt", "tables", ""},
		{"/dt", "tables", ""},
		{"/dn", "tables", ""},
		{"\\dn", "tables", ""},
		{"\\l", "databases", ""},
		{"\\l+", "databases", ""},
		{"/l", "databases", ""},
		{"\\d", "describe", ""},
		{"\\d users", "describe", "users"},
		{"\\d+ users", "describe", "users"},
		{"/d users", "describe", "users"},
		{"\\h", "help", ""},
		{"\\help", "help", ""},
		{"\\bogus", "unknown", "bogus"},
		// SQL, with trailing semicolons stripped (psql-style)
		{"SELECT * FROM t", "sql", "SELECT * FROM t"},
		{"SELECT * FROM t;", "sql", "SELECT * FROM t"},
		{"select count(*) from t;;", "sql", "select count(*) from t"},
		// empty / semicolon-only lines are no-ops, not errors
		{";", "noop", ""},
		{"", "noop", ""},
		{"   ", "noop", ""},
	}
	for _, c := range cases {
		kind, arg := classifyLine(c.line)
		if kind != c.kind || arg != c.arg {
			t.Errorf("classifyLine(%q) = (%q, %q), want (%q, %q)", c.line, kind, arg, c.kind, c.arg)
		}
	}
}
