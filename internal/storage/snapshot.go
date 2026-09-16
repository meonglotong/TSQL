package storage

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

// snapshotTable is the on-disk form of one table inside a snapshot.
type snapshotTable struct {
	ID     uint32
	Name   string
	Cols   []schema.Column
	Rows   map[int64][]types.Value
	NextID int64
}

// snapshotData is the full engine state captured at one sequence point.
type snapshotData struct {
	Seq    uint64
	Tables map[string]snapshotTable
}

type manifest struct {
	LatestSnapshot string `json:"latest_snapshot"`
}

// WriteSnapshot writes snapshot-<seq>.bin atomically (tmp + rename + fsync)
// and returns the file name.
func WriteSnapshot(dir string, d *snapshotData) (string, error) {
	final := filepath.Join(dir, fmt.Sprintf("snapshot-%d.bin", d.Seq))
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if err := gob.NewEncoder(f).Encode(d); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	syncDir(dir)
	return filepath.Base(final), nil
}

// LoadLatestSnapshot reads manifest.json and the snapshot it points at.
// A missing manifest or snapshot file yields (nil, nil).
func LoadLatestSnapshot(dir string) (*snapshotData, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.LatestSnapshot == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, m.LatestSnapshot))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var d snapshotData
	if err := gob.NewDecoder(f).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func writeManifest(dir, snapshotName string) error {
	b, err := json.MarshalIndent(manifest{LatestSnapshot: snapshotName}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
