package warmup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRejectsCorruptSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "warmup.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileStore{Path: p}).Load(); err == nil {
		t.Error("corrupt snapshot loaded without error")
	}
}

func TestRestoreRejectsOtherVersion(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 1000})
	if _, err := e.Restore(Snapshot{Version: SnapshotVersion + 1}); err == nil {
		t.Error("snapshot of another version restored")
	}
}

func TestFileStoreSaveFailsWhenPathIsADirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "warmup.json")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (FileStore{Path: p}).Save(Snapshot{}); err == nil {
		t.Error("save over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left %d entries, want only the directory: the temp file must be removed", len(entries))
	}
}
