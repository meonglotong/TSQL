package types

import (
	"testing"
	"time"
)

func TestValueString(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Null(), "NULL"},
		{Int(42), "42"},
		{Int(-7), "-7"},
		{Float(3.5), "3.5"},
		{Float(100), "100"},
		{Text("hello"), "hello"},
		{Bool(true), "true"},
		{Bool(false), "false"},
		{Time(time.Date(2026, 9, 14, 3, 52, 0, 0, time.UTC)), "2026-09-14 03:52:00"},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.want {
			t.Errorf("String(%v) = %q, want %q", c.v.Kind, got, c.want)
		}
	}
}

func TestCompareNumeric(t *testing.T) {
	if Int(1).Compare(Int(2)) != -1 {
		t.Error("1 should be < 2")
	}
	if Int(5).Compare(Int(5)) != 0 {
		t.Error("5 should equal 5")
	}
	// int vs float compare numerically
	if Int(2).Compare(Float(2.0)) != 0 {
		t.Error("int 2 should equal float 2.0")
	}
	if Int(2).Compare(Float(2.5)) != -1 {
		t.Error("2 should be < 2.5")
	}
}

func TestCompareKinds(t *testing.T) {
	// NULL sorts first
	if Null().Compare(Int(0)) != -1 {
		t.Error("NULL should sort before 0")
	}
	if Int(0).Compare(Null()) != 1 {
		t.Error("0 should sort after NULL")
	}
	if Null().Compare(Null()) != 0 {
		t.Error("NULL should equal NULL in ordering")
	}
	if Text("a").Compare(Text("b")) != -1 {
		t.Error("text a < b")
	}
	if Bool(false).Compare(Bool(true)) != -1 {
		t.Error("false < true")
	}
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if Time(t1).Compare(Time(t2)) != -1 {
		t.Error("t1 < t2")
	}
	// unrelated kinds: deterministic order by kind
	if Int(1).Compare(Text("a")) != -1 {
		t.Error("int should sort before text")
	}
}

func TestTypeCheck(t *testing.T) {
	ok := []struct {
		ct ColumnType
		v  Value
	}{
		{TypeInt, Int(1)},
		{TypeBigint, Int(1 << 40)},
		{TypeInt, Float(3.0)}, // integral float accepted
		{TypeFloat, Int(4)},
		{TypeText, Text("x")},
		{TypeBool, Bool(true)},
		{TypeBool, Int(1)},
		{TypeTimestamp, Time(time.Now().UTC())},
	}
	for _, c := range ok {
		if _, err := c.ct.Check(c.v); err != nil {
			t.Errorf("Check(%s, %v) unexpected error: %v", c.ct, c.v.Kind, err)
		}
	}
	bad := []struct {
		ct ColumnType
		v  Value
	}{
		{TypeInt, Float(3.5)},
		{TypeInt, Text("1")},
		{TypeText, Int(1)},
		{TypeText, Bool(true)},
		{TypeTimestamp, Text("2026-01-01 00:00:00")},
		{TypeBool, Text("true")},
	}
	for _, c := range bad {
		if _, err := c.ct.Check(c.v); err == nil {
			t.Errorf("Check(%s, %v) should fail", c.ct, c.v.Kind)
		}
	}
}
