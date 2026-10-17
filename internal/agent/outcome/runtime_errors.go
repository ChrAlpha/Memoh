package outcome

import (
	"log/slog"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/errs"
)

// RuntimeTurnError marks err as the failure of a turn runtimeType ran. Every
// External Agent failure carries its runtime under the same key, so one query
// over the result records finds them whichever agent failed and whatever code
// the failure took.
func RuntimeTurnError(runtimeType string, err error) error {
	return errs.WrapWithDepth(1, err, "external agent turn", slog.String("runtime", runtimeType))
}

// RuntimeHistoryError marks err as a failure to read the history a runtime run needs.
func RuntimeHistoryError(err error) error {
	if err == nil || apperror.CodeOf(err) != "" {
		return err
	}
	return apperror.Wrap(apperror.CodeSessionHistoryInconsistent, err, nil)
}

// RuntimePublishError names a failure to publish the run's live state to the
// Session Runtime. History is not involved (Redis down, run ownership lost),
// so it must not read as a history fault.
func RuntimePublishError(err error) error {
	if err == nil || apperror.CodeOf(err) != "" {
		return err
	}
	return apperror.Wrap(apperror.CodeSessionPublishFailed, err, nil)
}
