package migration

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestHashSetAddAndHas(t *testing.T) {
	set := newHashSet()
	first := common.HexToHash("0x01")
	second := common.HexToHash("0x02")

	if set.Has(first) {
		t.Fatal("new set contains first hash")
	}
	if !set.Add(first) {
		t.Fatal("first add was reported as a duplicate")
	}
	if !set.Has(first) {
		t.Fatal("added hash is missing")
	}
	if set.Add(first) {
		t.Fatal("duplicate add was reported as new")
	}
	if set.Has(second) {
		t.Fatal("set contains an unadded hash")
	}
	if !set.Add(second) || !set.Has(second) {
		t.Fatal("second hash was not added")
	}
}
