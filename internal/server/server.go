// Package server wires the wire protocol to the executor, one goroutine per
// connection.
package server

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	"github.com/meonglotong/tsql/internal/executor"
	"github.com/meonglotong/tsql/internal/parser"
	"github.com/meonglotong/tsql/internal/protocol"
	"github.com/meonglotong/tsql/internal/storage"
	"github.com/meonglotong/tsql/internal/types"
)

// Server serves TSQL connections over the framed protocol.
type Server struct {
	eng *storage.Engine
}

// New creates a server on top of an open engine.
func New(eng *storage.Engine) *Server { return &Server{eng: eng} }

// ListenAndServe serves on ln until the context is done. It returns nil on
// clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context, ln net.Listener) error {
	var conns []net.Conn
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				for _, c := range conns {
					c.Close()
				}
				return nil
			default:
			}
			return err
		}
		conns = append(conns, c)
		go s.handle(ctx, c)
	}
}

// connState is per-connection state: the socket plus the in-flight explicit
// transaction (nil when idle).
type connState struct {
	c   net.Conn
	txn *storage.Txn
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	st := &connState{c: c}
	defer func() {
		// A connection that dies mid-transaction aborts it (buffer discarded;
		// nothing was ever written to the WAL).
		if st.txn != nil {
			st.txn.Rollback()
		}
	}()
	remote := c.RemoteAddr().String()
	log.Printf("tsqld: client connected: %s", remote)
	defer log.Printf("tsqld: client disconnected: %s", remote)
	for {
		c.SetReadDeadline(time.Now().Add(10 * time.Minute))
		msg, err := protocol.ReadFrame(c)
		if err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.dispatch(st, msg)
	}
}

func (s *Server) dispatch(st *connState, msg protocol.Message) {
	switch msg.Type {
	case "query":
		s.runQuery(st, msg.SQL)
	case "tables":
		names := []string{}
		for _, d := range s.eng.Catalog() {
			names = append(names, d.Name)
		}
		protocol.WriteFrame(st.c, protocol.Message{Type: "tables", Tables: names})
		s.ready(st)
	default:
		protocol.WriteFrame(st.c, protocol.Message{Type: "error", Code: "0A000", Message: "unknown message type " + msg.Type})
		s.ready(st)
	}
}

// fail sends an error frame, aborting the connection's transaction if one is
// in flight (PG-like: an error aborts the current transaction).
func (s *Server) fail(st *connState, code, msg string) {
	if st.txn != nil {
		st.txn.Rollback()
		st.txn = nil
	}
	protocol.WriteFrame(st.c, protocol.Message{Type: "error", Code: code, Message: msg})
	s.ready(st)
}

func (s *Server) runQuery(st *connState, sql string) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		s.fail(st, "42601", err.Error())
		return
	}

	// Transaction control is owned by the server: the connection's txn
	// context is the read/write view the executor works through.
	switch stmt.(type) {
	case *parser.Begin:
		if st.txn != nil {
			// PG-compatible: a second BEGIN while a transaction is in
			// progress is a no-op warning, the first transaction continues.
			protocol.WriteFrame(st.c, protocol.Message{Type: "ok", Affected: 0})
			s.ready(st)
			return
		}
		txn, err := s.eng.Begin()
		if err != nil {
			s.fail(st, "58030", err.Error())
			return
		}
		st.txn = txn
		protocol.WriteFrame(st.c, protocol.Message{Type: "ok", Affected: 0})
		s.ready(st)
		return
	case *parser.Commit:
		if st.txn == nil {
			s.fail(st, "25006", "there is no transaction in progress")
			return
		}
		if err := st.txn.Commit(); err != nil {
			code, msg := "58030", err.Error()
			if errors.Is(err, storage.ErrUnique) {
				code, msg = "23505", "duplicate key value violates unique constraint"
			}
			s.fail(st, code, msg)
			return
		}
		st.txn = nil
		protocol.WriteFrame(st.c, protocol.Message{Type: "ok", Affected: 0})
		s.ready(st)
		return
	case *parser.Rollback:
		if st.txn == nil {
			s.fail(st, "25006", "there is no transaction in progress")
			return
		}
		st.txn.Rollback()
		st.txn = nil
		protocol.WriteFrame(st.c, protocol.Message{Type: "ok", Affected: 0})
		s.ready(st)
		return
	}

	// DDL is autocommit in v1 and cannot run inside an explicit transaction.
	if st.txn != nil {
		switch stmt.(type) {
		case *parser.CreateTable, *parser.DropTable:
			s.fail(st, "25000", "DDL is not supported inside an explicit transaction in v1")
			return
		}
	}

	acc := storage.Accessor(s.eng)
	if st.txn != nil {
		acc = st.txn
	}
	res, err := executor.Exec(s.eng, acc, stmt)
	if err != nil {
		code, text := "58030", err.Error()
		if sq, ok := err.(*executor.SQLError); ok {
			code, text = sq.Code, sq.Msg
		}
		s.fail(st, code, text)
		return
	}
	if res.Columns != nil {
		rows := make([][]any, len(res.Rows))
		for i, r := range res.Rows {
			rr := make([]any, len(r))
			for j, v := range r {
				rr[j] = toJSON(v)
			}
			rows[i] = rr
		}
		protocol.WriteFrame(st.c, protocol.Message{Type: "result", Columns: res.Columns, Rows: rows})
	} else {
		protocol.WriteFrame(st.c, protocol.Message{Type: "ok", Affected: res.Affected})
	}
	s.ready(st)
}

func (s *Server) ready(st *connState) {
	txn := "idle"
	if st.txn != nil {
		txn = "in-txn"
	}
	protocol.WriteFrame(st.c, protocol.Message{Type: "ready", Txn: txn})
}

func toJSON(v types.Value) any {
	switch v.Kind {
	case types.KindNull:
		return nil
	case types.KindInt:
		return v.I
	case types.KindFloat:
		return v.F
	case types.KindText:
		return v.S
	case types.KindBool:
		return v.B
	case types.KindTime:
		return v.T.UTC().Format("2006-01-02 15:04:05")
	}
	return nil
}
