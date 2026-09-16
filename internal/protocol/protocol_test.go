package protocol

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Message{Type: "query", SQL: "SELECT * FROM t WHERE x = 1"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.SQL != in.SQL {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestFrameResultPayload(t *testing.T) {
	var buf bytes.Buffer
	in := Message{Type: "result", Columns: []string{"a", "b"}, Rows: [][]any{{int64(1), "x"}, {nil, true}}}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Columns) != 2 || len(out.Rows) != 2 {
		t.Fatalf("payload lost: %+v", out)
	}
	if out.Rows[0][1] != "x" || out.Rows[1][0] != nil {
		t.Errorf("values: %+v", out.Rows)
	}
}

func TestFrameGarbageSize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF}) // 4 GiB length
	if _, err := ReadFrame(&buf); err == nil {
		t.Error("expected size error")
	}
}
