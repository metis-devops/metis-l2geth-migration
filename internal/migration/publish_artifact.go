package migration

import (
	"context"
	"fmt"
)

// artifactReportCodec keeps each report's strict decoder and equality rules
// independent while sharing the publication ordering and cancellation boundary.
type artifactReportCodec[T any] struct {
	label string
	write func(string, T) error
	load  func(string) (T, error)
	equal func(T, T) bool
}

// publishArtifact does not own output cleanup. The operation's deferred Abort
// removes only unpublished work, including after report or input-check failures.
func publishArtifact[T any](ctx context.Context, output *atomicDir, report T, codec artifactReportCodec[T], beforeCommit func() error, reporter *progressReporter) (retErr error) {
	phase := reporter.StartPhase("publish_artifact", nil, "output", output.final)
	defer func() { phase.Finish(retErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := codec.write(output.Path(), report); err != nil {
		return err
	}
	stored, err := codec.load(output.Path())
	if err != nil {
		return fmt.Errorf("re-open generated %s: %w", codec.label, err)
	}
	if !codec.equal(stored, report) {
		return fmt.Errorf("re-opened %s does not match generated report", codec.label)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return output.Commit()
}
