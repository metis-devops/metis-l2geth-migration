// Package strictio provides bounded, no-alias reads for versioned migration
// inputs. It deliberately rejects symbolic links and unexpected file types.
package strictio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxMetadataSize is the maximum accepted manifest or verification report.
const MaxMetadataSize int64 = 1 << 20

// EntryKind describes an exact top-level layout entry.
type EntryKind uint8

const (
	// RegularFile requires a non-symlink regular file.
	RegularFile EntryKind = iota + 1
	// Directory requires a non-symlink directory.
	Directory
)

// Root is an opened directory whose identity was checked without following a
// symbolic link. Paths opened through it cannot escape the directory tree.
type Root struct {
	path string
	root *os.Root
}

// OpenRoot opens path as a real directory and rejects a symbolic-link root.
func OpenRoot(path string) (*Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect input root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("input root %s must not be a symbolic link", path)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("input root %s is not a directory", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open input root: %w", err)
	}
	opened, err := root.Lstat(".")
	if err != nil {
		openErr := fmt.Errorf("inspect opened input root: %w", err)
		if closeErr := root.Close(); closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("close rejected input root: %w", closeErr))
		}
		return nil, openErr
	}
	if !os.SameFile(info, opened) {
		openErr := errors.New("input root changed while it was being opened")
		if closeErr := root.Close(); closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("close rejected input root: %w", closeErr))
		}
		return nil, openErr
	}
	return &Root{path: path, root: root}, nil
}

// Close releases the root handle.
func (r *Root) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	err := r.root.Close()
	r.root = nil
	if err != nil {
		return fmt.Errorf("close input root %s: %w", r.path, err)
	}
	return nil
}

// RequireExactLayout rejects missing, extra, symbolic-link, and wrong-kind
// top-level entries.
func (r *Root) RequireExactLayout(expected map[string]EntryKind) (retErr error) {
	dir, err := r.root.Open(".")
	if err != nil {
		return fmt.Errorf("open input root for inventory: %w", err)
	}
	defer func() {
		if err := dir.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close input-root inventory: %w", err))
		}
	}()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read input-root inventory: %w", err)
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("input root contains %d entries, want exactly %d", len(entries), len(expected))
	}
	for _, entry := range entries {
		kind, ok := expected[entry.Name()]
		if !ok {
			return fmt.Errorf("input root contains unexpected entry %q", entry.Name())
		}
		if err := r.requireKind(entry.Name(), kind); err != nil {
			return err
		}
	}
	return nil
}

func (r *Root) requireKind(name string, kind EntryKind) error {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid top-level entry name %q", name)
	}
	info, err := r.root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect input entry %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("input entry %q must not be a symbolic link", name)
	}
	switch kind {
	case RegularFile:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("input entry %q is not a regular file", name)
		}
	case Directory:
		if !info.IsDir() {
			return fmt.Errorf("input entry %q is not a directory", name)
		}
	default:
		return fmt.Errorf("input entry %q has unknown expected kind %d", name, kind)
	}
	return nil
}

// OpenRegular opens name after checking that it is a stable regular file and
// not a symbolic link. The caller owns the returned file.
func (r *Root) OpenRegular(name string) (*os.File, os.FileInfo, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, nil, fmt.Errorf("invalid regular-file name %q", name)
	}
	before, err := r.root.Lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect input file %q: %w", name, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("input file %q must be a regular file, not a symbolic link", name)
	}
	file, err := r.root.Open(name)
	if err != nil {
		return nil, nil, fmt.Errorf("open input file %q: %w", name, err)
	}
	after, err := file.Stat()
	if err != nil {
		openErr := fmt.Errorf("inspect opened input file %q: %w", name, err)
		if closeErr := file.Close(); closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("close rejected input file %q: %w", name, closeErr))
		}
		return nil, nil, openErr
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		openErr := fmt.Errorf("input file %q changed while it was being opened", name)
		if closeErr := file.Close(); closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("close rejected input file %q: %w", name, closeErr))
		}
		return nil, nil, openErr
	}
	return file, after, nil
}

// ReadRegular reads a stable regular file with an explicit maximum size.
func (r *Root) ReadRegular(name string, maximum int64) (data []byte, retErr error) {
	if maximum < 0 {
		return nil, errors.New("maximum read size is negative")
	}
	file, info, err := r.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close input file %q: %w", name, err))
		}
	}()
	if info.Size() > maximum {
		return nil, fmt.Errorf("input file %q is %d bytes, maximum is %d", name, info.Size(), maximum)
	}
	data, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read input file %q: %w", name, err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("input file %q exceeds maximum size %d", name, maximum)
	}
	return data, nil
}

// DecodeJSON decodes exactly one strict JSON value.
func DecodeJSON[T any](data []byte, label string) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode %s: trailing JSON value", label)
		}
		return value, fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return value, nil
}
