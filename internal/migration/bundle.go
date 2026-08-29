package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// BundleResult contains the verified manifest, record evidence, and rebuilt state.
type BundleResult struct {
	Manifest       bundle.Manifest
	ManifestBytes  []byte
	ManifestSHA256 common.Hash
	State          StateResult
	Records        bundle.ScanResult
}

// ScanBundle strictly validates a bundle and rebuilds its committed state root.
func ScanBundle(ctx context.Context, dir string, visitor StateVisitor) (BundleResult, error) {
	return scanBundle(ctx, dir, visitor, nil)
}

func scanBundle(ctx context.Context, dir string, visitor StateVisitor, progress *progressReporter) (result BundleResult, retErr error) {
	manifest, manifestBytes, err := bundle.LoadManifest(dir)
	if err != nil {
		return BundleResult{}, err
	}
	progress.Info("Bundle manifest loaded",
		"phase", "load_manifest",
		"status", "completed",
		"block", manifest.Source.HeadBefore.BlockNumber,
		"hash", manifest.Source.HeadBefore.BlockHash,
		"root", manifest.Source.HeadBefore.StateRoot,
		"compression", manifest.StateFile.Compression,
		"state_file_size", manifest.StateFile.Size,
	)
	var (
		counts         *progressCounts
		progressView   progressSnapshot
		trackedVisitor = visitor
	)
	if progress.Enabled() {
		counts = new(progressCounts)
		progressView = countProgressSnapshot(counts, &manifest.Counts)
		trackedVisitor = newCountingStateVisitor(visitor, counts)
	}
	phaseAttrs := append([]any{"bundle", dir}, totalCountAttrs(manifest.Counts)...)
	phase := progress.StartPhase("scan_bundle", progressView, phaseAttrs...)
	var finishAttrs []any
	defer func() {
		phase.Finish(retErr, finishAttrs...)
	}()
	consumer := &recordConsumer{
		expectedRoot: manifest.Source.HeadBefore.StateRoot,
		visitor:      trackedVisitor,
		accountStack: trie.NewStackTrie(nil),
	}
	records, err := bundle.ScanRecords(ctx, dir, manifest, consumer.consume)
	if err != nil {
		return BundleResult{}, err
	}
	state, err := consumer.finish()
	if err != nil {
		return BundleResult{}, err
	}
	if state.Counts != manifest.Counts {
		return BundleResult{}, fmt.Errorf("semantic counts mismatch: have %+v want %+v", state.Counts, manifest.Counts)
	}
	finishAttrs = []any{"recomputed_root", state.Root}
	return BundleResult{
		Manifest:       manifest,
		ManifestBytes:  manifestBytes,
		ManifestSHA256: bundle.ManifestSHA256(manifestBytes),
		State:          state,
		Records:        records,
	}, nil
}

type recordConsumer struct {
	expectedRoot common.Hash
	visitor      StateVisitor
	accountStack *trie.StackTrie
	counts       bundle.Counts

	haveAccount bool
	accountHash common.Hash
	account     *types.StateAccount
	accountRLP  []byte
	storage     *trie.StackTrie
	lastAccount common.Hash
	lastSlot    common.Hash
	haveSlot    bool
	codeSeen    bool
	finished    bool
}

func (c *recordConsumer) consume(record bundle.Record) error {
	if c.finished {
		return errors.New("record received after semantic scanner finished")
	}
	switch record.Type {
	case bundle.RecordAccount:
		if err := c.finalizeAccount(); err != nil {
			return err
		}
		if c.counts.Accounts > 0 && bytes.Compare(record.AccountHash[:], c.lastAccount[:]) <= 0 {
			return fmt.Errorf("account records are not strictly increasing: %s after %s", record.AccountHash, c.lastAccount)
		}
		account, canonical, err := decodeFullAccount(record.Payload)
		if err != nil {
			return fmt.Errorf("account %s: %w", record.AccountHash, err)
		}
		if !bytes.Equal(canonical, record.Payload) {
			return fmt.Errorf("account %s uses a non-canonical v1.17.5 account encoding", record.AccountHash)
		}
		c.haveAccount = true
		c.accountHash = record.AccountHash
		c.lastAccount = record.AccountHash
		c.account = account
		c.accountRLP = record.Payload
		c.storage = trie.NewStackTrie(nil)
		c.haveSlot = false
		c.codeSeen = false
		if c.visitor != nil {
			if err := c.visitor.Account(record.AccountHash, account, record.Payload); err != nil {
				return err
			}
		}
		c.addCount(bundle.RecordAccount, len(record.Payload))
	case bundle.RecordStorage:
		if !c.haveAccount {
			return errors.New("storage record appears before an account record")
		}
		if c.codeSeen {
			return fmt.Errorf("account %s storage record appears after its code record", c.accountHash)
		}
		if record.AccountHash != c.accountHash {
			return fmt.Errorf("storage record belongs to %s while current account is %s", record.AccountHash, c.accountHash)
		}
		if c.haveSlot && bytes.Compare(record.SubHash[:], c.lastSlot[:]) <= 0 {
			return fmt.Errorf("account %s storage records are not strictly increasing: %s after %s", c.accountHash, record.SubHash, c.lastSlot)
		}
		if err := validateStorageRLP(record.Payload); err != nil {
			return fmt.Errorf("account %s slot %s: %w", c.accountHash, record.SubHash, err)
		}
		if err := c.storage.Update(record.SubHash[:], record.Payload); err != nil {
			return fmt.Errorf("rebuild storage trie for account %s: %w", c.accountHash, err)
		}
		c.haveSlot = true
		c.lastSlot = record.SubHash
		if c.visitor != nil {
			if err := c.visitor.Storage(c.accountHash, record.SubHash, record.Payload); err != nil {
				return err
			}
		}
		c.addCount(bundle.RecordStorage, len(record.Payload))
	case bundle.RecordCode:
		if !c.haveAccount {
			return errors.New("code record appears before an account record")
		}
		if c.codeSeen {
			return fmt.Errorf("account %s has more than one code record", c.accountHash)
		}
		expected := common.BytesToHash(c.account.CodeHash)
		if expected == types.EmptyCodeHash {
			return fmt.Errorf("account %s has a code record but its code hash is empty", c.accountHash)
		}
		if record.SubHash != expected {
			return fmt.Errorf("account %s code hash mismatch: record %s account %s", c.accountHash, record.SubHash, expected)
		}
		if len(record.Payload) == 0 {
			return fmt.Errorf("account %s has empty contract code", c.accountHash)
		}
		if computed := crypto.Keccak256Hash(record.Payload); computed != expected {
			return fmt.Errorf("account %s code content hash mismatch: computed %s account %s", c.accountHash, computed, expected)
		}
		c.codeSeen = true
		if c.visitor != nil {
			if err := c.visitor.Code(c.accountHash, expected, record.Payload); err != nil {
				return err
			}
		}
		c.addCount(bundle.RecordCode, len(record.Payload))
	default:
		return fmt.Errorf("unknown semantic record type %d", record.Type)
	}
	return nil
}

func (c *recordConsumer) finalizeAccount() error {
	if !c.haveAccount {
		return nil
	}
	computed := c.storage.Hash()
	if computed != c.account.Root {
		return fmt.Errorf("account %s storage root mismatch: computed %s account %s", c.accountHash, computed, c.account.Root)
	}
	expectsCode := common.BytesToHash(c.account.CodeHash) != types.EmptyCodeHash
	if expectsCode != c.codeSeen {
		return fmt.Errorf("account %s code record presence mismatch: expected %t got %t", c.accountHash, expectsCode, c.codeSeen)
	}
	if err := c.accountStack.Update(c.accountHash[:], c.accountRLP); err != nil {
		return fmt.Errorf("rebuild account trie: %w", err)
	}
	c.haveAccount = false
	c.account = nil
	c.accountRLP = nil
	c.storage = nil
	return nil
}

func (c *recordConsumer) finish() (StateResult, error) {
	if c.finished {
		return StateResult{}, errors.New("semantic scanner is already finished")
	}
	c.finished = true
	if err := c.finalizeAccount(); err != nil {
		return StateResult{}, err
	}
	root := c.accountStack.Hash()
	if root != c.expectedRoot {
		return StateResult{}, fmt.Errorf("bundle state root mismatch: computed %s manifest %s", root, c.expectedRoot)
	}
	return StateResult{Root: root, Counts: c.counts}, nil
}

func (c *recordConsumer) addCount(typ byte, payloadLen int) {
	c.counts.Records++
	c.counts.PayloadBytes += uint64(payloadLen)
	switch typ {
	case bundle.RecordAccount:
		c.counts.Accounts++
	case bundle.RecordStorage:
		c.counts.StorageSlots++
	case bundle.RecordCode:
		c.counts.CodeReferences++
	}
}
