package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPublishArtifactFailureBoundaries(t *testing.T) {
	injected := errors.New("injected publication failure")
	for _, stage := range []string{"success", "canceled", "write", "load", "mismatch", "inputs", "cancel-inputs", "rename", "parent-sync"} {
		t.Run(stage, func(t *testing.T) {
			final := filepath.Join(t.TempDir(), "artifact")
			output, err := newAtomicDir(final)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := output.Abort(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if stage == "canceled" {
				cancel()
			}
			if stage == "rename" {
				output.ops.rename = func(string, string) error { return injected }
			}
			if stage == "parent-sync" {
				output.ops.syncDir = func(path string) error {
					if path == output.parent {
						return injected
					}
					return syncDirectory(path)
				}
			}
			var calls []string
			codec := artifactReportCodec[string]{
				label: "test report",
				write: func(dir, report string) error {
					calls = append(calls, "write")
					if stage == "write" {
						return injected
					}
					path := filepath.Join(dir, VerificationFileName)
					if err := os.WriteFile(path, []byte(report), 0600); err != nil {
						return err
					}
					return syncFile(path)
				},
				load: func(dir string) (string, error) {
					calls = append(calls, "load")
					if stage == "load" {
						return "", injected
					}
					if stage == "mismatch" {
						return "different", nil
					}
					data, err := os.ReadFile(filepath.Join(dir, VerificationFileName))
					return string(data), err
				},
				equal: func(a, b string) bool { return a == b },
			}
			err = publishArtifact(ctx, output, "report", codec, func() error {
				calls = append(calls, "inputs")
				if stage == "inputs" {
					return injected
				}
				if stage == "cancel-inputs" {
					cancel()
				}
				return nil
			}, nil)
			switch stage {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			case "canceled", "cancel-inputs":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case "mismatch":
				if err == nil {
					t.Fatal("accepted changed report")
				}
			default:
				if !errors.Is(err, injected) {
					t.Fatalf("lost failure: %v", err)
				}
			}
			wantCalls := []string{"write", "load", "inputs"}
			switch stage {
			case "canceled":
				wantCalls = nil
			case "write":
				wantCalls = wantCalls[:1]
			case "load", "mismatch":
				wantCalls = wantCalls[:2]
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("publication order %v, want %v", calls, wantCalls)
			}
			if stage == "parent-sync" {
				if _, ok := errors.AsType[*PublicationDurabilityError](err); !ok {
					t.Fatalf("lost durability classification: %v", err)
				}
			}
			if err := output.Abort(); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(final)
			if stage == "success" || stage == "parent-sync" {
				if statErr != nil {
					t.Fatalf("published output removed: %v", statErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed operation published: %v", statErr)
			}
			if _, err := os.Stat(output.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial survived: %v", err)
			}
		})
	}
}
