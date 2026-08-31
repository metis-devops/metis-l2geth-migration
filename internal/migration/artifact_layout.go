package migration

import (
	"errors"
	"fmt"

	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

func validateArtifactLayout(dir string) (retErr error) {
	root, err := strictio.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := root.RequireExactLayout(map[string]strictio.EntryKind{
		"db":                 strictio.Directory,
		VerificationFileName: strictio.RegularFile,
	}); err != nil {
		return fmt.Errorf("validate artifact layout: %w", err)
	}
	return nil
}

func loadArtifactJSON[T any](dir, label string, validate func(T) error) (result T, retErr error) {
	root, err := strictio.OpenRoot(dir)
	if err != nil {
		return result, fmt.Errorf("open artifact: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := root.RequireExactLayout(map[string]strictio.EntryKind{
		"db":                 strictio.Directory,
		VerificationFileName: strictio.RegularFile,
	}); err != nil {
		return result, fmt.Errorf("validate artifact layout: %w", err)
	}
	data, err := root.ReadRegular(VerificationFileName, strictio.MaxMetadataSize)
	if err != nil {
		return result, fmt.Errorf("read %s: %w", label, err)
	}
	result, err = strictio.DecodeJSON[T](data, label)
	if err != nil {
		return result, err
	}
	if err := validate(result); err != nil {
		return result, err
	}
	return result, nil
}
