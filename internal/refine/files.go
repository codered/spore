package refine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// readTarget returns a file's content, or nil when it does not exist.
func readTarget(path string) (*string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: fact paths come from memory.Path, notes paths from Config.AgentPath
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// sameContent compares two optional contents; nil equals only nil.
func sameContent(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// writeTarget replaces a file atomically, or removes it when content is nil.
// The temp name starts with a dot so memory.Load never reads a half-written
// fact.
func writeTarget(path string, content *string) error {
	if content == nil {
		err := os.Remove(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".refine-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(*content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func ptr(s string) *string { return &s }
