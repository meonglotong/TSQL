package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/meonglotong/tsql/internal/types"
)

func TestWALRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, recs, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected empty wal, got %d records", len(recs))
	}
	wanted := []Record{
		{Type: RecBegin, Txn: 1},
		{Type: RecPut, Txn: 1, TableID: 7, RowID: 42, Row: []types.Value{types.Int(1), types.Text("hi"), types.Bool(true)}},
		{Type: RecCommit, Txn: 1},
		{Type: RecBegin, Txn: 2},
		{Type: RecDelete, Txn: 2, TableID: 7, RowID: 42},
		{Type: RecCommit, Txn: 2},
	}
	for _, r := range wanted {
		if _, err := w.Append(r, r.Type == RecCommit); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	w2, got, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if len(got) != len(wanted) {
		t.Fatalf("got %d records, want %d", len(got), len(wanted))
	}
	for i := range wanted {
		if got[i].Type != wanted[i].Type || got[i].Txn != wanted[i].Txn ||
			got[i].TableID != wanted[i].TableID || got[i].RowID != wanted[i].RowID {
			t.Errorf("record %d: got type=%d txn=%d table=%d row=%d, want %+v",
				i, got[i].Type, got[i].Txn, got[i].TableID, got[i].RowID, wanted[i])
		}
	}
	for i, v := range wanted[1].Row {
		if got[1].Row[i].Compare(v) != 0 {
			t.Errorf("row value %d mismatch: %v vs %v", i, got[1].Row[i], v)
		}
	}
	if w2.Seq != 6 {
		t.Errorf("seq = %d, want 6", w2.Seq)
	}
}

func TestWALTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Append(Record{Type: RecBegin, Txn: 1}, false)
	w.Append(Record{Type: RecPut, Txn: 1, TableID: 1, RowID: 1, Row: []types.Value{types.Int(1)}}, false)
	w.Append(Record{Type: RecCommit, Txn: 1}, true)
	w.Close()

	info, _ := os.Stat(path)
	validSize := info.Size()

	// corrupt the tail with a torn record
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Write([]byte{0x02, 0x01, 0x00})
	f.Close()

	w2, got, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if len(got) != 3 {
		t.Fatalf("got %d records after torn tail, want 3", len(got))
	}
	info2, _ := os.Stat(path)
	if info2.Size() != validSize {
		t.Errorf("wal not truncated to valid prefix: %d != %d", info2.Size(), validSize)
	}
}

func TestWALCRCMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Append(Record{Type: RecCommit, Txn: 1}, true)
	w.Close()

	// flip one byte inside the record -> crc mismatch
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b[10] ^= 0xFF
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	w2, got, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if len(got) != 0 {
		t.Errorf("expected 0 valid records after corruption, got %d", len(got))
	}
}
