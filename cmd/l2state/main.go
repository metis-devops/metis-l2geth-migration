package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/ethereum/go-ethereum/log"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/migration"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

const (
	defaultCacheMB = 512
	defaultHandles = 256
)

type artifactFlags struct {
	output  *string
	scheme  *string
	engine  *string
	cache   *int
	handles *int
	quiet   *bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", version.ToolName, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		if err := printUsage(stderr); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
		return errors.New("a command is required")
	}
	switch args[0] {
	case "export":
		return normalizeHelp(runExport(ctx, args[1:], stdout, stderr))
	case "import":
		return normalizeHelp(runImport(ctx, args[1:], stdout, stderr))
	case "migrate":
		return normalizeHelp(runMigrate(ctx, args[1:], stdout, stderr))
	case "prune":
		return normalizeHelp(runPrune(ctx, args[1:], stdout, stderr))
	case "verify":
		return normalizeHelp(runVerify(ctx, args[1:], stdout, stderr))
	case "version":
		_, err := fmt.Fprintf(stdout, "%s %s (go-ethereum %s)\n", version.ToolName, version.ToolVersion, version.GethVersion)
		return err
	case "help", "-h", "--help":
		return printUsage(stdout)
	case "sleep": // do nothing, useful for debugging
		<-ctx.Done()
		return nil
	default:
		if err := printUsage(stderr); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func normalizeHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source-chaindata", "", "stopped l2geth LevelDB chaindata directory")
	target := addArtifactFlags(flags)
	workers := flags.Int("workers", defaultMigrateWorkers(), "global account/storage workers; values below 2 are raised to 2, maximum 16")
	ovmEnabled := flags.Bool("migrate-ovm-eth", false, "convert OVM ETH balances and create a migration checkpoint")
	wrappedCode := flags.String("wrapped-ether-code", "", "storage-compatible wrappedEther runtime bytecode hex file")
	ancient := flags.String("source-ancient", "", "legacy ancient directory (default: source-chaindata/ancient)")
	witness := flags.String("ovm-state-witness", "", "OVM address/allowance ownership JSONL file")
	alloc := flags.String("ovm-genesis-alloc", "", "GenesisAlloc JSON overrides applied after OVM balance conversion")
	if err := parseFlags(flags, args, "migrate"); err != nil {
		return err
	}
	result, err := migration.Migrate(ctx, migration.MigrateOptions{
		SourceChaindata: *source,
		Output:          *target.output,
		Scheme:          *target.scheme,
		DBEngine:        *target.engine,
		CacheMB:         *target.cache,
		Handles:         *target.handles,
		Workers:         *workers,
		Progress:        newProgressOptions(stderr, *target.quiet),
		OVM:             migration.OVMOptions{Enabled: *ovmEnabled, WrappedEtherCode: *wrappedCode, SourceAncient: *ancient, StateWitness: *witness, GenesisAlloc: *alloc},
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func runPrune(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("chaindata", "", "stopped legacy full-node LevelDB to prune in place; LES service databases are unsupported")
	temp := flags.String("temp-dir", "", "existing parent for temporary state database, outside chaindata (default: chaindata parent)")
	cache := flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB, minimum 32")
	handles := flags.Int("handles", defaultHandles, "database file handle allowance, minimum 32")
	workers := flags.Int("workers", defaultMigrateWorkers(), "global account/storage/hash workers; minimum 2, maximum 16")
	dryRun := flags.Bool("dry-run", false, "validate and count candidates without modifying chaindata")
	compact := flags.Bool("compact", false, "manually compact after pruning to reclaim disk space")
	quiet := flags.Bool("quiet", false, "disable progress logs on stderr")
	if err := parseFlags(flags, args, "prune"); err != nil {
		return err
	}
	result, err := migration.Prune(ctx, migration.PruneOptions{
		Chaindata: *path, TempDir: *temp, CacheMB: *cache, Handles: *handles, Workers: *workers,
		DryRun: *dryRun, Compact: *compact, Progress: newProgressOptions(stderr, *quiet),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source-chaindata", "", "stopped l2geth LevelDB chaindata directory")
	output := flags.String("out", "", "new bundle directory")
	compression := flags.String("compression", bundle.CompressionZstd, "record compression: zstd or none")
	cache := flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB")
	handles := flags.Int("handles", defaultHandles, "database file handle allowance")
	quiet := flags.Bool("quiet", false, "disable progress logs on stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("export does not accept positional arguments")
	}
	result, err := migration.Export(ctx, migration.ExportOptions{
		SourceChaindata: *source,
		Output:          *output,
		Compression:     *compression,
		CacheMB:         *cache,
		Handles:         *handles,
		Progress:        newProgressOptions(stderr, *quiet),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bundlePath := flags.String("bundle", "", "export bundle directory")
	target := addArtifactFlags(flags)
	if err := parseFlags(flags, args, "import"); err != nil {
		return err
	}
	result, err := migration.Import(ctx, migration.ImportOptions{
		Bundle:   *bundlePath,
		Output:   *target.output,
		Scheme:   *target.scheme,
		DBEngine: *target.engine,
		CacheMB:  *target.cache,
		Handles:  *target.handles,
		Progress: newProgressOptions(stderr, *target.quiet),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bundlePath := flags.String("bundle", "", "export bundle directory")
	source := flags.String("source-chaindata", "", "stopped l2geth LevelDB chaindata directory")
	artifact := flags.String("artifact", "", "state artifact directory; optional with --bundle")
	cache := flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB")
	handles := flags.Int("handles", defaultHandles, "database file handle allowance")
	quiet := flags.Bool("quiet", false, "disable progress logs on stderr")
	wrappedCode := flags.String("wrapped-ether-code", "", "original wrappedEther runtime bytecode hex file for OVM verification")
	ancient := flags.String("source-ancient", "", "legacy ancient directory for OVM verification")
	witness := flags.String("ovm-state-witness", "", "original OVM ownership JSONL file")
	alloc := flags.String("ovm-genesis-alloc", "", "original GenesisAlloc JSON overrides for OVM verification")
	workers := flags.Int("workers", defaultMigrateWorkers(), "global workers for OVM verification, maximum 16")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("verify does not accept positional arguments")
	}
	if (*bundlePath == "") == (*source == "") {
		return errors.New("verify requires exactly one of --bundle or --source-chaindata")
	}
	if *source != "" {
		if *artifact == "" {
			return errors.New("--artifact is required with --source-chaindata")
		}
		format, err := migration.ArtifactVerificationFormat(*artifact)
		if err != nil {
			return err
		}
		if format == migration.OVMVerificationFormat {
			report, err := migration.VerifyOVM(ctx, migration.OVMVerifyOptions{SourceChaindata: *source, Artifact: *artifact, CacheMB: *cache, Handles: *handles, Workers: *workers,
				OVM: migration.OVMOptions{Enabled: true, WrappedEtherCode: *wrappedCode, SourceAncient: *ancient, StateWitness: *witness, GenesisAlloc: *alloc}, Progress: newProgressOptions(stderr, *quiet)})
			if err != nil {
				return err
			}
			return writeJSON(stdout, report)
		}
		if *wrappedCode != "" || *ancient != "" || *witness != "" || *alloc != "" {
			return errors.New("OVM input flags require an OVM artifact")
		}
		report, err := migration.VerifyDirect(ctx, migration.DirectVerifyOptions{
			SourceChaindata: *source,
			Artifact:        *artifact,
			CacheMB:         *cache,
			Handles:         *handles,
			Progress:        newProgressOptions(stderr, *quiet),
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, report)
	}
	if *wrappedCode != "" || *ancient != "" || *witness != "" || *alloc != "" {
		return errors.New("OVM input flags cannot be used with --bundle")
	}
	report, err := migration.Verify(ctx, migration.VerifyOptions{
		Bundle:   *bundlePath,
		Artifact: *artifact,
		CacheMB:  *cache,
		Handles:  *handles,
		Progress: newProgressOptions(stderr, *quiet),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func addArtifactFlags(flags *flag.FlagSet) artifactFlags {
	return artifactFlags{
		output:  flags.String("out", "", "new state artifact directory"),
		engine:  flags.String("db-engine", "pebble", "target database engine: pebble or leveldb"),
		scheme:  flags.String("scheme", "", "target state scheme: hash or path"),
		cache:   flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB"),
		handles: flags.Int("handles", defaultHandles, "database file handle allowance"),
		quiet:   flags.Bool("quiet", false, "disable progress logs on stderr"),
	}
}

func defaultMigrateWorkers() int {
	return max(2, min(4, runtime.GOMAXPROCS(0)))
}

func parseFlags(flags *flag.FlagSet, args []string, command string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s does not accept positional arguments", command)
	}
	for _, name := range []string{"db-engine"} {
		if option := flags.Lookup(name); option != nil && option.Value.String() == "" {
			return fmt.Errorf("--%s must not be empty", name)
		}
	}
	return nil
}

func newProgressOptions(stderr io.Writer, quiet bool) migration.ProgressOptions {
	if quiet {
		return migration.ProgressOptions{}
	}
	logger := log.NewLogger(log.NewTerminalHandlerWithLevel(stderr, log.LevelInfo, false))
	return migration.ProgressOptions{Logger: logger}
}

func printUsage(w io.Writer) error {
	_, err := fmt.Fprintf(w, `Usage:
  l2state export --source-chaindata PATH --out BUNDLE [--compression zstd|none] [--quiet]
  l2state import --bundle BUNDLE --out ARTIFACT --scheme hash|path [--db-engine pebble|leveldb] [--quiet]
  l2state migrate --source-chaindata PATH --out ARTIFACT --scheme hash|path [--db-engine pebble|leveldb] [--workers N] [--quiet]
  l2state prune --chaindata PATH [--workers N] [--temp-dir PATH] [--dry-run | --compact] [--quiet]
  l2state verify --bundle BUNDLE [--artifact ARTIFACT] [--quiet]
  l2state verify --source-chaindata PATH --artifact ARTIFACT [--quiet]
  l2state version
  l2state sleep

The source must be a stopped l2geth LevelDB or a consistent filesystem copy.
Outputs must not already exist. Artifacts contain chaindata/ and verification.json.
Artifacts contain state only and are not bootable geth chaindata.
Prune is an offline in-place operation for legacy full-node LevelDB only.
It retains latest executed state plus the genesis root node and all non-state records.
LES service databases are unsupported. Default pruning does not manually compact.
`)
	return err
}
