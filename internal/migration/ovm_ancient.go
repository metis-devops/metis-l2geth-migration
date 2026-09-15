package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/golang/snappy"
)

// legacyAncient opens only existing files with O_RDONLY. In particular it never
// uses a freezer constructor (which may repair/truncate files or create locks).
type legacyAncient struct {
	path   string
	tables [3]*legacyAncientTable
	count  uint64
}

type legacyAncientTable struct {
	index      *os.File
	name       string
	compressed bool
	count      uint64
	indexInfo  os.FileInfo
}

func openLegacyAncient(path string, explicit bool) (a *legacyAncient, retErr error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect legacy ancient: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("legacy ancient must be a real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	a = &legacyAncient{path: path}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, a.Close())
		}
	}()
	for n, name := range []string{"hashes", "headers", "receipts"} {
		t, err := openLegacyAncientTable(path, name, n != 0)
		if err != nil {
			return a, err
		}
		a.tables[n] = t
		if n == 0 {
			a.count = t.count
		} else if a.count != t.count {
			return a, errors.New("legacy ancient table lengths disagree")
		}
	}
	return a, nil
}

func openLegacyAncientTable(path, name string, compressed bool) (*legacyAncientTable, error) {
	ext := ".ridx"
	if compressed {
		ext = ".cidx"
	}
	f, err := openOVMInput(filepath.Join(path, name+ext))
	if err != nil {
		return nil, fmt.Errorf("open ancient %s index: %w", name, err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if info.Size() < 6 || info.Size()%6 != 0 {
		return nil, errors.Join(errors.New("malformed ancient index length"), f.Close())
	}
	var first [6]byte
	if _, err = f.ReadAt(first[:], 0); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if binary.BigEndian.Uint32(first[2:]) != 0 {
		return nil, errors.Join(errors.New("ancient history has a truncated tail"), f.Close())
	}
	return &legacyAncientTable{index: f, name: name, compressed: compressed, count: uint64(info.Size()/6 - 1), indexInfo: info}, nil
}

func (a *legacyAncient) read(table int, number uint64) (blob []byte, retErr error) {
	if a == nil || number >= a.count {
		return nil, nil
	}
	t := a.tables[table]
	var offsets [12]byte
	if _, err := t.index.ReadAt(offsets[:], int64(number*6)); err != nil {
		return nil, fmt.Errorf("read ancient %s index: %w", t.name, err)
	}
	startFile, endFile := binary.BigEndian.Uint16(offsets[:2]), binary.BigEndian.Uint16(offsets[6:8])
	start, end := binary.BigEndian.Uint32(offsets[2:6]), binary.BigEndian.Uint32(offsets[8:])
	if endFile < startFile || uint32(endFile) > uint32(startFile)+1 {
		return nil, errors.New("ancient file sequence is invalid")
	}
	if startFile != endFile {
		start = 0
	}
	if end < start {
		return nil, errors.New("ancient offsets are reversed")
	}
	ext := ".rdat"
	if t.compressed {
		ext = ".cdat"
	}
	f, err := openOVMInput(filepath.Join(a.path, fmt.Sprintf("%s.%04d%s", t.name, endFile, ext)))
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if int64(end) > info.Size() {
		return nil, errors.New("ancient data file is truncated")
	}
	blob = make([]byte, int(end-start))
	if _, err = f.ReadAt(blob, int64(start)); err != nil && (!errors.Is(err, io.EOF) || len(blob) != 0) {
		return nil, err
	}
	if t.compressed {
		blob, err = snappy.Decode(nil, blob)
		if err != nil {
			return nil, fmt.Errorf("decode ancient %s snappy: %w", t.name, err)
		}
	}
	return blob, nil
}

func (a *legacyAncient) Close() error {
	if a == nil {
		return nil
	}
	var result error
	for _, t := range a.tables {
		if t == nil || t.index == nil {
			continue
		}
		info, err := t.index.Stat()
		if err == nil && (info.Size() != t.indexInfo.Size() || !info.ModTime().Equal(t.indexInfo.ModTime())) {
			err = errors.New("ancient index changed during migration")
		}
		result = errors.Join(result, err, t.index.Close())
		t.index = nil
	}
	return result
}

func mergeLegacyHistoryValue(hot, cold []byte) ([]byte, error) {
	if len(hot) != 0 && len(cold) != 0 && !bytes.Equal(hot, cold) {
		return nil, errors.New("hot and ancient canonical history disagree")
	}
	if len(cold) != 0 {
		return cold, nil
	}
	if len(hot) == 0 {
		return nil, errors.New("canonical history record is missing")
	}
	return hot, nil
}
