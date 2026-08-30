package migration

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// writeHeadMetadata stores the selected source header and the minimal lookup
// and head markers needed to identify it. No body, receipts, TD, or history are
// written.
func writeHeadMetadata(db ethdb.Database, source bundle.SourceEvidence) error {
	header, err := validatedSourceHeader(source)
	if err != nil {
		return err
	}
	batch := db.NewBatch()
	defer batch.Close()
	writeHeadMetadataEntries(batch, header, source.HeadBefore)
	if err := batch.Write(); err != nil {
		return fmt.Errorf("write target head metadata: %w", err)
	}
	return nil
}

func verifyHeadMetadata(db ethdb.Database, source bundle.SourceEvidence) error {
	if _, err := validatedSourceHeader(source); err != nil {
		return err
	}
	head := source.HeadBefore
	headerRLP := rawdb.ReadHeaderRLP(db, head.BlockHash, head.BlockNumber)
	if len(headerRLP) == 0 {
		return errors.New("artifact head header is missing")
	}
	if !bytes.Equal(headerRLP, source.HeaderRLP) {
		return errors.New("artifact head header RLP does not match source evidence")
	}
	number, ok := rawdb.ReadHeaderNumber(db, head.BlockHash)
	if !ok {
		return errors.New("artifact head hash-to-number mapping is missing")
	}
	if number != head.BlockNumber {
		return fmt.Errorf("artifact head hash-to-number mapping is %d, want %d", number, head.BlockNumber)
	}
	if canonical := rawdb.ReadCanonicalHash(db, head.BlockNumber); canonical != head.BlockHash {
		return fmt.Errorf("artifact canonical hash is %s, want %s", canonical, head.BlockHash)
	}
	if hash := rawdb.ReadHeadBlockHash(db); hash != head.BlockHash {
		return fmt.Errorf("artifact LastBlock is %s, want %s", hash, head.BlockHash)
	}
	if hash := rawdb.ReadHeadHeaderHash(db); hash != head.BlockHash {
		return fmt.Errorf("artifact LastHeader is %s, want %s", hash, head.BlockHash)
	}
	return nil
}

func validatedSourceHeader(source bundle.SourceEvidence) (*types.Header, error) {
	header, err := source.ValidatedHeader()
	if err != nil {
		return nil, fmt.Errorf("validate source head metadata: %w", err)
	}
	return header, nil
}

func writeHeadMetadataEntries(writer ethdb.KeyValueWriter, header *types.Header, head bundle.Head) {
	rawdb.WriteHeader(writer, header)
	rawdb.WriteCanonicalHash(writer, head.BlockHash, head.BlockNumber)
	rawdb.WriteHeadBlockHash(writer, head.BlockHash)
	rawdb.WriteHeadHeaderHash(writer, head.BlockHash)
}

type headMetadataEntries map[string][]byte

func expectedHeadMetadata(source bundle.SourceEvidence) (headMetadataEntries, error) {
	header, err := validatedSourceHeader(source)
	if err != nil {
		return nil, err
	}
	entries := make(headMetadataEntries)
	writeHeadMetadataEntries(entries, header, source.HeadBefore)
	return entries, nil
}

func (entries headMetadataEntries) Put(key, value []byte) error {
	entries[string(key)] = bytes.Clone(value)
	return nil
}

func (entries headMetadataEntries) Delete(key []byte) error {
	delete(entries, string(key))
	return nil
}
