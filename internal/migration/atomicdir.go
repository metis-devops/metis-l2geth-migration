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
	abs, err := resolvePathWithMissing(final)
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
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve output parent: %w", err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute output parent: %w", err)
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if _, err := os.Lstat(abs); err == nil {
		return nil, fmt.Errorf("output path already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect resolved output path: %w", err)
	}
	if err := verifyRenameNoReplace(parent); err != nil {
		return nil, err
	}
	prefix := "." + filepath.Base(abs) + ".partial-"
	temp, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return nil, fmt.Errorf("create temporary output: %w", err)
	}
	return &atomicDir{final: abs, parent: parent, temp: temp, tempPrefix: prefix}, nil
}

func verifyRenameNoReplace(parent string) (retErr error) {
	source, err := os.MkdirTemp(parent, ".l2state-rename-source-")
	if err != nil {
		return fmt.Errorf("create no-replace source probe: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(source); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove no-replace source probe: %w", err))
		}
	}()
	destination, err := os.MkdirTemp(parent, ".l2state-rename-destination-")
	if err != nil {
		return fmt.Errorf("create no-replace destination probe: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(destination); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove no-replace destination probe: %w", err))
		}
	}()
	err = renameNoReplace(source, destination)
	if err == nil {
		return errors.New("platform rename primitive replaced an existing directory")
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("atomic no-replace directory publication is unavailable: %w", err)
	}
	return nil
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
	if err := renameNoReplace(d.temp, d.final); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	d.committed = true
	return syncDirectory(d.parent)
}

func resolvePathWithMissing(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	probe := abs
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			components := append([]string{resolved}, missing...)
			return filepath.Join(components...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}
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
