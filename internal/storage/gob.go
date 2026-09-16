package storage

import (
	"bytes"
	"encoding/gob"
)

// gob helpers kept separate for readability at call sites.
func gobEncode(buf *bytes.Buffer, v any) error {
	return gob.NewEncoder(buf).Encode(v)
}

func gobDecode(r *bytes.Reader, v any) error {
	return gob.NewDecoder(r).Decode(v)
}
