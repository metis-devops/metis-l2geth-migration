package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
)

// Inputs were strictly parsed before replay. Confirmation hashes raw bytes
// without re-decoding them or writing the now-consumed evidence index.
func hashOVMInput(ctx context.Context, path string) (digest common.Hash, retErr error) {
	f, err := openOVMInput(path)
	if err != nil {
		return digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	return hashOVMInputReader(ctx, f)
}

func hashOVMInputReader(ctx context.Context, r io.Reader) (digest common.Hash, err error) {
	h := sha256.New()
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return digest, err
		}
		n, err := r.Read(buffer[:])
		h.Write(buffer[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return digest, fmt.Errorf("hash OVM input: %w", err)
			}
			copy(digest[:], h.Sum(nil))
			return digest, ctx.Err()
		}
	}
}

func confirmOVMInputs(ctx context.Context, opts OVMOptions, inputs ovmInputs) error {
	for _, input := range []struct {
		path, label string
		digest      common.Hash
	}{
		{opts.GenesisAlloc, "GenesisAlloc", inputs.allocDigest},
		{opts.WrappedEtherCode, "wrapped code", inputs.codeFileDigest},
		{opts.StateWitness, "OVM witness", inputs.witnessDigest},
	} {
		if input.path == "" {
			continue
		}
		digest, err := hashOVMInput(ctx, input.path)
		if err != nil {
			return fmt.Errorf("confirm %s input: %w", input.label, err)
		}
		if digest != input.digest {
			return fmt.Errorf("%s input changed during operation", input.label)
		}
	}
	return ctx.Err()
}

func confirmOVMReportInputs(ctx context.Context, opts OVMOptions, report OVMVerificationReport) error {
	if (report.GenesisAlloc != nil) != (opts.GenesisAlloc != "") {
		return errors.New("OVM verification requires the original --ovm-genesis-alloc input exactly when recorded")
	}
	inputs := ovmInputs{codeFileDigest: report.CodeFileSHA256, witnessDigest: report.WitnessSHA256}
	if report.GenesisAlloc != nil {
		inputs.allocDigest = report.GenesisAlloc.FileSHA256
	}
	return confirmOVMInputs(ctx, opts, inputs)
}
