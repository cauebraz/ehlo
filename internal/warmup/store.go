package warmup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Store persists snapshots. A production deployment replaces FileStore
// with a database-backed implementation.
type Store interface {
	Load() (Snapshot, error)
	Save(Snapshot) error
}

// FileStore writes the snapshot to a JSON file with an atomic rename.
type FileStore struct {
	Path string
}

// Load reads the snapshot. A missing file is an empty snapshot.
func (f FileStore) Load() (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("load snapshot %s: %w", f.Path, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("decode snapshot %s: %w", f.Path, err)
	}
	return s, nil
}

// Save writes the snapshot to a temp file in the same directory, syncs it
// and renames it over Path. The state is the server's own, so the
// directory and file are private to its user.
func (f FileStore) Save(s Snapshot) (err error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("snapshot dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*.json")
	if err != nil {
		return fmt.Errorf("snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(b); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err = os.Rename(tmpName, f.Path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return syncDir(dir)
}

// syncDir makes the rename durable: without it, a power loss can bring
// back the previous snapshot.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open snapshot dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync snapshot dir: %w", err)
	}
	return nil
}

// NopStore keeps nothing. For tests and for deployments that accept
// restarting every lane.
type NopStore struct{}

// Load returns an empty snapshot.
func (NopStore) Load() (Snapshot, error) { return Snapshot{}, nil }

// Save discards the snapshot.
func (NopStore) Save(Snapshot) error { return nil }
