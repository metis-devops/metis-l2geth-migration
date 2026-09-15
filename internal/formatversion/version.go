// Package formatversion owns the independently versioned l2state contracts.
package formatversion

import "fmt"

const (
	// Bundle versions the portable manifest and record encoding.
	Bundle = 1
	// Verification versions bundle-backed reports and their target layouts.
	Verification = 1
	// DirectVerification versions direct reports and their target layouts.
	DirectVerification = 1
	// Prune versions the independent offline pruning result.
	Prune = 1
	// OVMVerification versions the opt-in balance-conversion checkpoint report.
	OVMVerification = 1
)

// RecordChainDomain binds record-chain evidence to the portable format version.
func RecordChainDomain() string {
	return fmt.Sprintf("metis-l2state-record-chain/v%d", Bundle)
}
