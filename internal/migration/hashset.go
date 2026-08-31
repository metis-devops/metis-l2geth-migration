package migration

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// hashSet is an exact operation-local set of hashes.
type hashSet map[common.Hash]struct{}

func newHashSet() hashSet {
	return make(hashSet)
}

// Add records hash and reports whether it was absent before this call.
func (s hashSet) Add(hash common.Hash) bool {
	if s.Has(hash) {
		return false
	}
	s[hash] = struct{}{}
	return true
}

// Has reports whether hash was already recorded.
func (s hashSet) Has(hash common.Hash) bool {
	_, ok := s[hash]
	return ok
}

type concurrentHashSet struct {
	mu     sync.Mutex
	hashes hashSet
}

func newConcurrentHashSet() *concurrentHashSet {
	return &concurrentHashSet{hashes: newHashSet()}
}

func (s *concurrentHashSet) Add(hash common.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hashes.Add(hash)
}
