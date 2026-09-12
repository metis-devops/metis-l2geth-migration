package migration

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

// pruneManifestStorage retains the underlying filesystem lock and normal write
// operations, but never calls fileStorage.GetMeta: even a read-only DB session
// would let that method restore CURRENT when the storage owns a writable lock.
type pruneManifestStorage struct {
	storage.Storage
	path string
}

func (s pruneManifestStorage) GetMeta() (fd storage.FileDesc, retErr error) {
	if err := walkPruneFiles(s.path, rejectPendingPruneCurrent); err != nil {
		return fd, err
	}
	root, err := strictio.OpenRoot(s.path)
	if err != nil {
		return fd, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	// A generated manifest filename is at most 28 bytes, plus its newline.
	current, err := root.ReadRegular("CURRENT", 64)
	if err != nil {
		return fd, fmt.Errorf("read prune CURRENT without fallback: %w", err)
	}
	fd, err = parsePruneCurrent(string(current))
	if err != nil {
		return fd, err
	}
	manifest, _, err := root.OpenRegular(fd.String())
	if err != nil {
		return storage.FileDesc{}, fmt.Errorf("open manifest named by prune CURRENT without fallback: %w", err)
	}
	if err := manifest.Close(); err != nil {
		return storage.FileDesc{}, fmt.Errorf("close inspected prune manifest: %w", err)
	}
	return fd, nil
}

func parsePruneCurrent(current string) (storage.FileDesc, error) {
	name, newline := strings.CutSuffix(current, "\n")
	number, prefix := strings.CutPrefix(name, "MANIFEST-")
	n, err := strconv.ParseInt(number, 10, 64)
	fd := storage.FileDesc{Type: storage.TypeManifest, Num: n}
	if !newline || !prefix || err != nil || n < 0 || name != fd.String() {
		return storage.FileDesc{}, errors.New("invalid prune CURRENT; refusing manifest backup fallback or repair")
	}
	return fd, nil
}

func rejectPendingPruneCurrent(entry os.DirEntry) error {
	suffix, ok := strings.CutPrefix(entry.Name(), "CURRENT.")
	if !ok || suffix == "bak" {
		return nil
	}
	// These are the pending-rename names recognized by goleveldb's GetMeta.
	if _, err := strconv.ParseInt(suffix, 10, 64); err == nil {
		return fmt.Errorf("pending LevelDB manifest publication %q; refusing prune metadata recovery", entry.Name())
	}
	return nil
}
