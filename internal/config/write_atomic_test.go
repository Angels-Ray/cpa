package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicReplacesWithoutEmptyWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := []byte("port: 8317\ndebug: true\n")
	if err := WriteFileAtomic(path, payload, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
}
