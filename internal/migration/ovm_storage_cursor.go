package migration

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/ethdb"
)

// Storage and evidence keys have the same hash ordering. Reuse one iterator
// per immutable evidence namespace instead of seeking three times per slot.
// Sparse witnesses must not turn a lookup into an unbounded evidence scan.
const ovmEvidenceCursorSteps = 8

type ovmStorageCursor struct {
	db     ethdb.Database
	prefix byte
	it     ethdb.Iterator
	valid  bool
}

func (c *ovmStorageCursor) close() {
	if c.it != nil {
		c.it.Release()
		c.it = nil
	}
}

// get requires strictly increasing, fixed-size storage keys. Returned bytes
// belong to the iterator and remain valid only until the next call or close.
func (c *ovmStorageCursor) get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if c.it == nil {
		c.seek(key)
	}
	for steps := 0; c.valid && bytes.Compare(c.it.Key()[1:], key) < 0; steps++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if steps == ovmEvidenceCursorSteps {
			c.close()
			c.seek(key)
			break
		}
		c.valid = c.it.Next()
	}
	if err := c.it.Error(); err != nil {
		return nil, false, fmt.Errorf("iterate OVM evidence %c: %w", c.prefix, err)
	}
	if c.valid && bytes.Equal(c.it.Key()[1:], key) {
		return c.it.Value(), true, nil
	}
	return nil, false, nil
}

func (c *ovmStorageCursor) seek(key []byte) {
	c.it = c.db.NewIterator([]byte{c.prefix}, key)
	c.valid = c.it.Next()
}
