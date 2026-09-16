// Package types holds TSQL value types, comparisons, and type checking.
package types

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Kind identifies the dynamic type of a Value.
type Kind uint8

const (
	KindNull Kind = iota
	KindInt
	KindFloat
	KindText
	KindBool
	KindTime
)

// ColumnType names the v1 column types.
type ColumnType string

const (
	TypeInt       ColumnType = "int"
	TypeBigint    ColumnType = "bigint"
	TypeFloat     ColumnType = "float"
	TypeText      ColumnType = "text"
	TypeBool      ColumnType = "bool"
	TypeTimestamp ColumnType = "timestamp"
)

// ValidTypes lists the column types accepted by the v1 catalog.
var ValidTypes = map[ColumnType]bool{
	TypeInt: true, TypeBigint: true, TypeFloat: true,
	TypeText: true, TypeBool: true, TypeTimestamp: true,
}

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindText:
		return "text"
	case KindBool:
		return "bool"
	case KindTime:
		return "timestamp"
	}
	return "unknown"
}

// Value is one SQL cell. The zero value is NULL.
// KindInt covers both `int` and `bigint` columns (int64 range).
type Value struct {
	Kind Kind
	I    int64
	F    float64
	S    string
	B    bool
	T    time.Time
}

func Null() Value            { return Value{} }
func Int(i int64) Value      { return Value{Kind: KindInt, I: i} }
func Float(f float64) Value  { return Value{Kind: KindFloat, F: f} }
func Text(s string) Value    { return Value{Kind: KindText, S: s} }
func Bool(b bool) Value      { return Value{Kind: KindBool, B: b} }
func Time(t time.Time) Value { return Value{Kind: KindTime, T: t} }

func (v Value) IsNull() bool { return v.Kind == KindNull }

// String renders the value in TSQL output format.
func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindInt:
		return fmt.Sprintf("%d", v.I)
	case KindFloat:
		return fmt.Sprintf("%g", v.F)
	case KindText:
		return v.S
	case KindBool:
		if v.B {
			return "true"
		}
		return "false"
	case KindTime:
		return v.T.UTC().Format("2006-01-02 15:04:05")
	}
	return "NULL"
}

// Compare returns -1, 0, or 1. It is a total order: NULL sorts first,
// KindInt and KindFloat compare numerically against each other, and
// unrelated kinds fall back to kind order then value. SQL's NULL semantics
// for `=` comparisons are handled by the executor, NOT here.
func (v Value) Compare(o Value) int {
	if v.IsNull() && o.IsNull() {
		return 0
	}
	if v.IsNull() {
		return -1
	}
	if o.IsNull() {
		return 1
	}
	if (v.Kind == KindInt || v.Kind == KindFloat) && (o.Kind == KindInt || o.Kind == KindFloat) {
		a, b := float64(v.I), float64(o.I)
		if v.Kind == KindFloat {
			a = v.F
		}
		if o.Kind == KindFloat {
			b = o.F
		}
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}
	if v.Kind != o.Kind {
		if v.Kind < o.Kind {
			return -1
		}
		return 1
	}
	switch v.Kind {
	case KindInt:
		return cmp.Compare(v.I, o.I)
	case KindText:
		return strings.Compare(v.S, o.S)
	case KindBool:
		if v.B == o.B {
			return 0
		}
		if !v.B {
			return -1
		}
		return 1
	case KindTime:
		switch {
		case v.T.Before(o.T):
			return -1
		case v.T.After(o.T):
			return 1
		default:
			return 0
		}
	}
	return 0
}

// Check validates (and coerces where safe) v for storage in a column of
// type ct. It returns the stored value or an error. NULL is always
// type-compatible; NOT NULL constraints are enforced by the executor.
func (ct ColumnType) Check(v Value) (Value, error) {
	if v.Kind == KindNull {
		return Null(), nil
	}
	switch ct {
	case TypeInt, TypeBigint:
		switch v.Kind {
		case KindInt:
			return v, nil
		case KindFloat:
			if v.F == float64(int64(v.F)) {
				return Int(int64(v.F)), nil
			}
		}
	case TypeFloat:
		switch v.Kind {
		case KindFloat:
			return v, nil
		case KindInt:
			return Float(float64(v.I)), nil
		}
	case TypeText:
		if v.Kind == KindText {
			return v, nil
		}
	case TypeBool:
		switch v.Kind {
		case KindBool:
			return v, nil
		case KindInt:
			return Bool(v.I != 0), nil
		}
	case TypeTimestamp:
		if v.Kind == KindTime {
			return v, nil
		}
	}
	return Null(), fmt.Errorf("cannot store %s value in %s column", v.Kind, ct)
}
