package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/meonglotong/tsql/internal/protocol"
	"github.com/meonglotong/tsql/internal/storage"
)

type client struct {
	t    *testing.T
	conn net.Conn
}

// query sends one statement and returns the payload + ready frames.
// Test-goroutine only (uses t.Fatalf).
func (cl *client) query(sql string) (protocol.Message, protocol.Message) {
	cl.t.Helper()
	if err := protocol.WriteFrame(cl.conn, protocol.Message{Type: "query", SQL: sql}); err != nil {
		cl.t.Fatalf("write %q: %v", sql, err)
	}
	payload, err := protocol.ReadFrame(cl.conn)
	if err != nil {
		cl.t.Fatalf("read payload for %q: %v", sql, err)
	}
	ready, err := protocol.ReadFrame(cl.conn)
	if err != nil {
		cl.t.Fatalf("read ready for %q: %v", sql, err)
	}
	if ready.Type != "ready" {
		cl.t.Fatalf("ready frame for %q = %+v", sql, ready)
	}
	return payload, ready
}

func (cl *client) expectOK(sql string, affected int) {
	m, _ := cl.query(sql)
	if m.Type != "ok" || m.Affected != affected {
		cl.t.Fatalf("expectOK %q: %+v", sql, m)
	}
}

func (cl *client) expectErr(sql, code string) {
	m, _ := cl.query(sql)
	if m.Type != "error" || m.Code != code {
		cl.t.Fatalf("expectErr %q: %+v", sql, m)
	}
}

// expectInt expects a single-row, single-column integer result (JSON decodes
// numbers as float64).
func (cl *client) expectInt(sql string, want int) {
	m, _ := cl.query(sql)
	if m.Type != "result" || len(m.Rows) != 1 || len(m.Rows[0]) != 1 {
		cl.t.Fatalf("expectInt %q: %+v", sql, m)
	}
	v, ok := m.Rows[0][0].(float64)
	if !ok || int(v) != want {
		cl.t.Fatalf("expectInt %q: %v", sql, m.Rows[0][0])
	}
}

func startServer(t *testing.T) string {
	t.Helper()
	eng, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(eng).ListenAndServe(ctx, ln)
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn}
}

func TestServerTxnLifecycle(t *testing.T) {
	addr := startServer(t)
	cl := dial(t, addr)
	cl.expectOK("CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, v TEXT)", 0)

	_, r := cl.query("BEGIN")
	if r.Txn != "in-txn" {
		t.Errorf("ready after BEGIN = %q, want in-txn", r.Txn)
	}
	cl.expectOK("BEGIN", 0) // second BEGIN is a no-op, first txn continues
	cl.expectOK("INSERT INTO t (v) VALUES ('a')", 1)

	// another connection: no dirty reads
	cl2 := dial(t, addr)
	cl2.expectInt("SELECT count(*) FROM t", 0)
	// same connection: read-your-own-writes
	cl.expectInt("SELECT count(*) FROM t", 1)

	cl.expectOK("COMMIT", 0)
	_, r = cl.query("COMMIT")
	if r.Txn != "idle" {
		t.Errorf("ready after COMMIT = %q, want idle", r.Txn)
	}
	r2, _ := cl.query("ROLLBACK")
	if r2.Type != "error" {
		t.Errorf("ROLLBACK without txn should error, got %+v", r2)
	}
	cl.expectErr("ROLLBACK", "25006")
	cl.expectInt("SELECT count(*) FROM t", 1)
	cl2.expectInt("SELECT count(*) FROM t", 1)
}

func TestServerTxnErrorAborts(t *testing.T) {
	addr := startServer(t)
	cl := dial(t, addr)
	cl.expectOK("CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, v TEXT UNIQUE)", 0)

	cl.expectOK("BEGIN", 0)
	cl.expectOK("INSERT INTO t (v) VALUES ('a')", 1)
	// unique violation inside the txn: error, and the txn is aborted
	cl.expectErr("INSERT INTO t (v) VALUES ('a')", "23505")
	// txn was aborted by the error: COMMIT now complains
	cl.expectErr("COMMIT", "25006")
	cl.expectInt("SELECT count(*) FROM t", 0)
}

func TestServerDDLRejectedInTxn(t *testing.T) {
	addr := startServer(t)
	cl := dial(t, addr)
	cl.expectOK("BEGIN", 0)
	cl.expectErr("CREATE TABLE x (id INT PRIMARY KEY)", "25000")
	// the failed DDL aborted the txn
	cl.expectErr("COMMIT", "25006")
}

func TestServerDisconnectMidTxn(t *testing.T) {
	addr := startServer(t)
	cl := dial(t, addr)
	cl.expectOK("CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, v TEXT)", 0)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cl2 := &client{t: t, conn: conn}
	cl2.expectOK("BEGIN", 0)
	cl2.expectOK("INSERT INTO t (v) VALUES ('ghost')", 1)
	conn.Close() // dies mid-transaction

	// committed state must never contain the uncommitted row
	cl.expectInt("SELECT count(*) FROM t", 0)
}

func TestServerUniqueConflictAtCommit(t *testing.T) {
	addr := startServer(t)
	a := dial(t, addr)
	b := dial(t, addr)
	a.expectOK("CREATE TABLE mail (id INT PRIMARY KEY AUTO_INCREMENT, addr TEXT UNIQUE)", 0)

	a.expectOK("BEGIN", 0)
	b.expectOK("BEGIN", 0)
	a.expectOK("INSERT INTO mail (addr) VALUES ('x@x')", 1)
	// b's write-time check passes (a not committed yet)...
	b.expectOK("INSERT INTO mail (addr) VALUES ('x@x')", 1)
	a.expectOK("COMMIT", 0)
	// ...but b's commit is rejected
	b.expectErr("COMMIT", "23505")
	// b's aborted txn is gone; its row never landed
	a.expectInt("SELECT count(*) FROM mail", 1)
}

// TestConcurrentClients is the Fase 3 exit criterion: N clients, no lost
// updates. Each client runs perClient BEGIN/INSERT/COMMIT cycles in its own
// transaction, then a read-modify-write UPDATE on its own rows.
func TestConcurrentClients(t *testing.T) {
	addr := startServer(t)
	c0 := dial(t, addr)
	c0.expectOK("CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, owner INT, v INT)", 0)

	const clients, per = 4, 25
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(owner int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("worker %d dial: %v", owner, err)
				return
			}
			defer conn.Close()
			q := func(sql string) (protocol.Message, error) {
				if err := protocol.WriteFrame(conn, protocol.Message{Type: "query", SQL: sql}); err != nil {
					return protocol.Message{}, err
				}
				m, err := protocol.ReadFrame(conn)
				if err != nil {
					return protocol.Message{}, err
				}
				if _, err := protocol.ReadFrame(conn); err != nil { // ready
					return protocol.Message{}, err
				}
				return m, nil
			}
			for i := 0; i < per; i++ {
				if m, err := q("BEGIN"); err != nil || m.Type != "ok" {
					t.Errorf("worker %d begin: %+v %v", owner, m, err)
					return
				}
				sql := fmt.Sprintf("INSERT INTO t (owner, v) VALUES (%d, %d)", owner, i)
				if m, err := q(sql); err != nil || m.Type != "ok" || m.Affected != 1 {
					t.Errorf("worker %d insert: %+v %v", owner, m, err)
					return
				}
				if m, err := q("COMMIT"); err != nil || m.Type != "ok" {
					t.Errorf("worker %d commit: %+v %v", owner, m, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()

	c0.expectInt("SELECT count(*) FROM t", clients*per)

	// read-modify-write: every row's v is incremented once, inside one txn
	c0.expectOK("BEGIN", 0)
	upd, _ := c0.query("UPDATE t SET v = v + 1 WHERE owner = 1")
	if upd.Type != "ok" || upd.Affected != per {
		t.Fatalf("update: %+v", upd)
	}
	c0.expectOK("COMMIT", 0)
	// sum = (0+1+...+24)*4 + 25 (owner 1 bumped) = 1200 + 25
	c0.expectInt("SELECT sum(v) FROM t", (0+1+2+3+4+5+6+7+8+9+10+11+12+13+14+15+16+17+18+19+20+21+22+23+24)*4+25)
}

func TestDescribeAndDatabases(t *testing.T) {
	addr := startServer(t)
	cl := dial(t, addr)
	cl.expectOK("CREATE TABLE users (id INT PRIMARY KEY AUTO_INCREMENT, name TEXT NOT NULL, score FLOAT UNIQUE)", 0)

	// \l -> databases (single database in v1)
	if err := protocol.WriteFrame(cl.conn, protocol.Message{Type: "databases"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	seenDB := false
	for {
		m, err := protocol.ReadFrame(cl.conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type == "databases" {
			seenDB = true
			if len(m.Tables) != 1 || m.Tables[0] != "tsql" {
				t.Errorf("databases = %v", m.Tables)
			}
		}
		if m.Type == "ready" {
			break
		}
	}
	if !seenDB {
		t.Fatal("no databases frame")
	}

	// \d users -> column listing
	if err := protocol.WriteFrame(cl.conn, protocol.Message{Type: "describe", Name: "users"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	seenDesc := false
	for {
		m, err := protocol.ReadFrame(cl.conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type == "describe" {
			seenDesc = true
			if m.Name != "users" {
				t.Errorf("describe name = %q", m.Name)
			}
			flat := ""
			for _, r := range m.Rows {
				for _, c := range r {
					flat += fmt.Sprint(c) + "|"
				}
			}
			if !strings.Contains(flat, "id") || !strings.Contains(flat, "PK") ||
				!strings.Contains(flat, "auto_increment") || !strings.Contains(flat, "UNI") {
				t.Errorf("describe rows = %v", m.Rows)
			}
		}
		if m.Type == "error" {
			t.Fatalf("unexpected error frame: %+v", m)
		}
		if m.Type == "ready" {
			break
		}
	}
	if !seenDesc {
		t.Fatal("no describe frame")
	}

	// \d nosuch -> 42P01
	if err := protocol.WriteFrame(cl.conn, protocol.Message{Type: "describe", Name: "nosuch"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		m, err := protocol.ReadFrame(cl.conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type == "error" {
			if m.Code != "42P01" {
				t.Errorf("describe nosuch code = %s, want 42P01", m.Code)
			}
			break
		}
		if m.Type == "ready" {
			break
		}
	}
}
