package migration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type atomicDir struct {
	final      string
	parent     string
	temp       string
	tempPrefix string
	committed  bool
}

func newAtomicDir(final string) (*atomicDir, error) {
	if final == "" {
		return nil, errors.New("output path is empty")
	}
	abs, err := filepath.Abs(final)
	if err != nil {
		return nil, fmt.Errorf("resolve output path: %w", err)
	}
	if _, err := os.Lstat(abs); err == nil {
		return nil, fmt.Errorf("output path already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect output path: %w", err)
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create output parent: %w", err)
	}
	prefix := "." + filepath.Base(abs) + ".partial-"
	temp, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return nil, fmt.Errorf("create temporary output: %w", err)
	}
	return &atomicDir{final: abs, parent: parent, temp: temp, tempPrefix: prefix}, nil
}

func (d *atomicDir) Path() string {
	return d.temp
}

func (d *atomicDir) Commit() error {
	if d.committed {
		return errors.New("output is already committed")
	}
	if _, err := os.Lstat(d.final); err == nil {
		return fmt.Errorf("output path appeared during operation: %s", d.final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output before publish: %w", err)
	}
	if err := syncDirectory(d.temp); err != nil {
		return err
	}
	if err := os.Rename(d.temp, d.final); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	d.committed = true
	return syncDirectory(d.parent)
}

func (d *atomicDir) Abort() error {
	if d == nil || d.committed || d.temp == "" {
		return nil
	}
	clean := filepath.Clean(d.temp)
	if filepath.Dir(clean) != d.parent || !strings.HasPrefix(filepath.Base(clean), d.tempPrefix) {
		return fmt.Errorf("refusing to remove unexpected temporary path %s", clean)
	}
	return os.RemoveAll(clean)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		syncErr := fmt.Errorf("sync directory %s: %w", path, err)
		if closeErr := dir.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("close directory %s: %w", path, closeErr))
		}
		return syncErr
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close directory %s: %w", path, err)
	}
	return nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		syncErr := fmt.Errorf("sync file %s: %w", path, err)
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("close file %s: %w", path, closeErr))
		}
		return syncErr
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close file %s: %w", path, err)
	}
	return nil
}
