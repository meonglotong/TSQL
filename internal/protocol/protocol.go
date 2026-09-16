// Package protocol implements the TSQL wire protocol:
//
//	[4-byte big-endian length][JSON payload]
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MaxFrame bounds a single frame (16 MiB).
const MaxFrame = 16 << 20

// Message is one wire message (client->server or server->client).
// Client->server: {"type":"query","sql":"..."} or {"type":"tables"}.
// Server->client: result | ok | error | ready | tables.
type Message struct {
	Type     string   `json:"type"`
	SQL      string   `json:"sql,omitempty"`
	Columns  []string `json:"columns,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"`
	Affected int      `json:"affected,omitempty"`
	Code     string   `json:"code,omitempty"`
	Message  string   `json:"message,omitempty"`
	Txn      string   `json:"txn,omitempty"`
	Tables   []string `json:"tables,omitempty"`
}

// WriteFrame writes one length-framed JSON message.
func WriteFrame(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("protocol: frame too large (%d bytes)", len(b))
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(b)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one length-framed JSON message.
func ReadFrame(r io.Reader) (Message, error) {
	var m Message
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return m, fmt.Errorf("protocol: frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return m, err
	}
	err := json.Unmarshal(buf, &m)
	return m, err
}
