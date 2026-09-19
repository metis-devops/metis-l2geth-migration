package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// verificationWorkspace owns only its unique scratch directory. Inputs remain
// read-only, including when TMPDIR or an explicit parent points into an input.
// Database owners must close their handles before Close removes the workspace.
type verificationWorkspace struct {
	parent string
	inputs []string
	path   string
}

func prepareVerificationWorkspace(parent string, inputs ...string) (*verificationWorkspace, error) {
	w := &verificationWorkspace{parent: parent, inputs: inputs}
	// Explicit configuration is checked even for verification without scratch.
	if parent != "" {
		if err := w.resolveParent(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *verificationWorkspace) resolveParent() error {
	parent := w.parent
	if parent == "" {
		parent = os.TempDir()
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve verification temp-dir: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("resolve absolute verification temp-dir: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect verification temp-dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("verification temp-dir must be an existing directory")
	}
	if err := w.rejectInputs(resolved); err != nil {
		return err
	}
	w.parent = resolved
	return nil
}

func (w *verificationWorkspace) rejectInputs(path string) error {
	for _, input := range w.inputs {
		if input == "" {
			continue
		}
		if err := rejectOutputInsideDirectory(input, path,
			"verification temp-dir must be outside input directories",
			"verification temp-dir aliases an input directory"); err != nil {
			return fmt.Errorf("check verification scratch against %s: %w", input, err)
		}
	}
	return nil
}

func (w *verificationWorkspace) create(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w.path != "" {
		return w.path, nil
	}
	if err := w.resolveParent(); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(w.parent, ".l2state-verify-")
	if err != nil {
		return "", fmt.Errorf("create verification workspace: %w", err)
	}
	w.path = path
	if err := w.rejectInputs(path); err != nil {
		return "", errors.Join(err, w.Close())
	}
	return path, nil
}

func (w *verificationWorkspace) nodeIndex(ctx context.Context, mode TempDBMode, scheme string, cache, handles int) (trieNodeIndexOptions, error) {
	opts := trieNodeIndexOptions{Mode: mode, CacheMB: cache, Handles: handles}
	if mode.normalized() == TempDBDisk && scheme == "hash" {
		var err error
		opts.Parent, err = w.create(ctx)
		return opts, err
	}
	return opts, nil
}

func (w *verificationWorkspace) Close() error {
	if w == nil || w.path == "" {
		return nil
	}
	if err := os.RemoveAll(w.path); err != nil {
		return fmt.Errorf("remove verification workspace: %w", err)
	}
	w.path = ""
	return nil
}
