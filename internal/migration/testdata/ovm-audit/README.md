# mvm audit summary v1 wire fixtures

Schema source: MetisProtocol/mvm branch `audit-ovm-balance`, commit
`d497d8ee`, `l2geth/core/ovm_audit.go` (`finish`) and
`l2geth/core/state/ovm_audit.go` (`OVMAuditScan`, `OVMAuditDelta`).

These are synthetic, manually constructed wire fixtures bound to the existing
`../ovm-conversion/source.json`, not output of a mainnet execution audit.
They do not certify execution or historical causes. The diagnostic variant
adds both optional informational fields (including a false match) to exercise
their decoding; it is not evidence for the known mainnet target. Neither the
legacy canary nor any frozen compatibility corpus was regenerated.
