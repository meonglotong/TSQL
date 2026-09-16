package storage

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

// Record types in the write-ahead log.
type RecType uint8

const (
	RecBegin  RecType = 1 // payload: none
	RecPut    RecType = 2 // payload: tableID u32, rowID i64, gob([]types.Value)
	RecDelete RecType = 3 // payload: tableID u32, rowID i64
	RecCommit RecType = 4 // payload: none; durable point
	RecCreate RecType = 5 // payload: gob(SchemaChange)
	RecDrop   RecType = 6 // payload: gob(SchemaChange)
)

const maxPayload = 64 << 20 // 64 MiB sanity bound per record

var errMalformed = errors.New("tsql: malformed wal record")

// SchemaChange is the DDL payload logged in the WAL.
type SchemaChange struct {
	Create bool
	Name   string
	Cols   []schema.Column
}

// Record is one decoded WAL entry.
type Record struct {
	Type    RecType
	Txn     uint64
	TableID uint32
	RowID   int64
	Row     []types.Value
	DDL     *SchemaChange
}

// Record wire format:
//
//	[type 1B][txn 8B LE][payload len 4B LE][payload][crc32 4B LE]
//
// The CRC covers everything before it. A torn or corrupt record ends replay;
// valid records before it are kept.
func encodeRecord(r Record) ([]byte, error) {
	var payload []byte
	switch r.Type {
	case RecBegin, RecCommit:
	case RecPut:
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.LittleEndian, r.TableID); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint64(r.RowID)); err != nil {
			return nil, err
		}
		if err := gobEncode(&buf, r.Row); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
	case RecDelete:
		b := make([]byte, 12)
		binary.LittleEndian.PutUint32(b[0:4], r.TableID)
		binary.LittleEndian.PutUint64(b[4:12], uint64(r.RowID))
		payload = b
	case RecCreate, RecDrop:
		if r.DDL == nil {
			return nil, errors.New("tsql: ddl record missing payload")
		}
		var buf bytes.Buffer
		if err := gobEncode(&buf, r.DDL); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
	default:
		return nil, fmt.Errorf("tsql: unknown record type %d", r.Type)
	}
	buf := make([]byte, 13+len(payload)+4)
	buf[0] = byte(r.Type)
	binary.LittleEndian.PutUint64(buf[1:9], r.Txn)
	binary.LittleEndian.PutUint32(buf[9:13], uint32(len(payload)))
	copy(buf[13:], payload)
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], crc32.ChecksumIEEE(buf[:len(buf)-4]))
	return buf, nil
}

func decodeRecord(hdr, payload []byte) (Record, error) {
	rec := Record{Type: RecType(hdr[0]), Txn: binary.LittleEndian.Uint64(hdr[1:9])}
	switch rec.Type {
	case RecBegin, RecCommit:
	case RecPut, RecDelete:
		if len(payload) < 12 {
			return rec, errMalformed
		}
		rec.TableID = binary.LittleEndian.Uint32(payload[0:4])
		rec.RowID = int64(binary.LittleEndian.Uint64(payload[4:12]))
		if rec.Type == RecPut {
			if err := gobDecode(bytes.NewReader(payload[12:]), &rec.Row); err != nil {
				return rec, errMalformed
			}
		}
	case RecCreate, RecDrop:
		sc := &SchemaChange{}
		if err := gobDecode(bytes.NewReader(payload), sc); err != nil {
			return rec, errMalformed
		}
		rec.DDL = sc
	default:
		return rec, errMalformed
	}
	return rec, nil
}

// ReadRecords decodes records from f starting at its current offset.
// It returns the decoded records and the byte offset of the first invalid
// (torn/corrupt) or missing record. err is non-nil only for real I/O errors.
func ReadRecords(f *os.File) ([]Record, int64, error) {
	recs := []Record{}
	var off int64
	hdr := make([]byte, 13)
	for {
		start := off
		if _, err := io.ReadFull(f, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return recs, start, err
		}
		off += 13
		plen := int(binary.LittleEndian.Uint32(hdr[9:13]))
		if plen > maxPayload {
			return recs, start, nil // corrupt header -> stop
		}
		payload := make([]byte, plen)
		if _, err := io.ReadFull(f, payload); err != nil {
			return recs, start, nil // torn tail
		}
		off += int64(plen)
		crcb := make([]byte, 4)
		if _, err := io.ReadFull(f, crcb); err != nil {
			return recs, start, nil // torn tail
		}
		off += 4
		want := binary.LittleEndian.Uint32(crcb)
		if got := crc32.ChecksumIEEE(append(hdr, payload...)); got != want {
			return recs, start, nil // crc mismatch -> stop
		}
		rec, err := decodeRecord(hdr, payload)
		if err != nil {
			return recs, start, nil
		}
		recs = append(recs, rec)
	}
	return recs, off, nil
}

// WAL is an append-only write-ahead log. Writes are serialized; the caller
// decides which record is durable (Commit fsyncs, earlier records piggyback).
type WAL struct {
	path string
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	Seq  uint64 // records currently in this file
}

// OpenWAL opens (creating if needed) the WAL at path, truncates any torn tail,
// and returns the surviving records.
func OpenWAL(path string) (*WAL, []Record, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, err
	}
	recs, validLen, rerr := ReadRecords(f)
	if rerr != nil {
		f.Close()
		return nil, nil, rerr
	}
	size, _ := f.Seek(0, io.SeekEnd)
	if size > validLen {
		if err := f.Truncate(validLen); err != nil {
			f.Close()
			return nil, nil, err
		}
	}
	f.Seek(0, io.SeekEnd)
	return &WAL{path: path, f: f, w: bufio.NewWriter(f), Seq: uint64(len(recs))}, recs, nil
}

// Append writes one record. If durable, the file is fsynced (commit point).
func (w *WAL) Append(r Record, durable bool) (uint64, error) {
	buf, err := encodeRecord(r)
	if err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(buf); err != nil {
		return 0, err
	}
	if err := w.w.Flush(); err != nil {
		return 0, err
	}
	if durable {
		if err := w.f.Sync(); err != nil {
			return 0, err
		}
	}
	w.Seq++
	return w.Seq, nil
}

// Truncate empties the WAL (used after a snapshot checkpoint).
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w.Seq = 0
	return nil
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}
