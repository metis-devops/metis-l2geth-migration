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
	"syscall"

	"github.com/ethereum/go-ethereum/log"
	"github.com/metis-devops/metis-l2geth-migration/internal/migration"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

const (
	defaultCacheMB = 512
	defaultHandles = 256
)

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
		return runExport(ctx, args[1:], stdout, stderr)
	case "import":
		return runImport(ctx, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr)
	case "version":
		_, err := fmt.Fprintf(stdout, "%s %s (go-ethereum %s %s)\n", version.ToolName, version.ToolVersion, version.GethVersion, version.GethCommit)
		return err
	case "help", "-h", "--help":
		return printUsage(stdout)
	default:
		if err := printUsage(stderr); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source-chaindata", "", "stopped l2geth LevelDB chaindata directory")
	output := flags.String("out", "", "new bundle directory")
	compression := flags.String("compression", "zstd", "record compression: zstd or none")
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
	return writeJSON(stdout, struct {
		Bundle   string `json:"bundle"`
		Manifest any    `json:"manifest"`
	}{Bundle: result.BundlePath, Manifest: result.Manifest})
}

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bundlePath := flags.String("bundle", "", "export bundle directory")
	output := flags.String("out", "", "new state artifact directory")
	scheme := flags.String("scheme", "", "target state scheme: hash or path")
	cache := flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB")
	handles := flags.Int("handles", defaultHandles, "database file handle allowance")
	quiet := flags.Bool("quiet", false, "disable progress logs on stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("import does not accept positional arguments")
	}
	result, err := migration.Import(ctx, migration.ImportOptions{
		Bundle:   *bundlePath,
		Output:   *output,
		Scheme:   *scheme,
		CacheMB:  *cache,
		Handles:  *handles,
		Progress: newProgressOptions(stderr, *quiet),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Artifact string                       `json:"artifact"`
		Report   migration.VerificationReport `json:"verification"`
	}{Artifact: result.ArtifactPath, Report: result.Report})
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bundlePath := flags.String("bundle", "", "export bundle directory")
	artifact := flags.String("artifact", "", "optional imported state artifact directory")
	cache := flags.Int("cache-mb", defaultCacheMB, "database cache allowance in MiB")
	handles := flags.Int("handles", defaultHandles, "database file handle allowance")
	quiet := flags.Bool("quiet", false, "disable progress logs on stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("verify does not accept positional arguments")
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
  l2state import --bundle BUNDLE --out ARTIFACT --scheme hash|path [--quiet]
  l2state verify --bundle BUNDLE [--artifact ARTIFACT] [--quiet]
  l2state version

The source must be a stopped l2geth LevelDB or a consistent filesystem copy.
Outputs must not already exist. Artifacts contain state only and are not bootable geth chaindata.
`)
	return err
}
