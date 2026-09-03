package server

import (
	"context"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/setupapply"
	"github.com/pterodactyl/wings/remote"
)

// SetupApplyStatus is the payload of every "setup apply status" websocket
// event. Error and ErrorCode are absent from a successful attempt's events;
// ErrorCode is one of the SetupApplyError constants so the panel branches
// on a stable code rather than a message.
type SetupApplyStatus struct {
	SetupID   string `json:"setup_id"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

// The stable error codes a failed native setup apply reports on both the
// terminal status event and the panel callback. The first three name the
// step that failed; timeout means the job's own deadline expired; and
// internal_error covers a cancelled job or a recovered panic. The set is a
// contract with the panel and must only ever grow.
const (
	SetupApplyErrorStopFailed  = "stop_failed"
	SetupApplyErrorApplyFailed = "apply_failed"
	SetupApplyErrorStartFailed = "start_failed"
	SetupApplyErrorTimeout     = "timeout"
	SetupApplyErrorInternal    = "internal_error"
)

// setupApplyStopGrace is how long the job lets the game stop on its own
// before terminating it. A server mid-setup has minutes of play behind it
// at most, and the user pressed Start knowing it restarts.
const setupApplyStopGrace = 60 * time.Second

// setupApplyStartWait bounds how long the start step waits for the power
// lock, matching what the power route allows a client to ask for.
const setupApplyStartWait = 30

// setupApplySettleInterval is how often the stop step re-reads the
// environment state while waiting for a stop to settle.
const setupApplySettleInterval = 100 * time.Millisecond

// setupApplyTimeout resolves the deadline one attempt runs under. It reads
// the configured value on every call rather than caching it, and is a
// variable rather than a plain expression only so a test can shorten a
// bound whose configured granularity is whole minutes.
var setupApplyTimeout = func() time.Duration {
	return time.Duration(config.Get().System.SetupApply.TimeoutMinutes) * time.Minute
}

// AdmitSetupApply admits one native setup apply attempt in a single
// critical section: a repeat of the running or most recently finished id
// reports repeat true without claiming, otherwise the install reservation
// is claimed and the id recorded together, or ErrOperationInProgress is
// returned when another operation holds the server. The install kind is
// reused because it already cancels SFTP sessions and blocks power actions.
func (s *Server) AdmitSetupApply(id string) (bool, error) {
	return s.admitFenced(&s.operation.setup, OperationInstall, id)
}

// ActiveSetupApplyID reports the setup_id of the apply currently running
// against this server, or the empty string.
func (s *Server) ActiveSetupApplyID() string {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()
	return s.operation.setup.active
}

// releaseSetupApplyClaim retires a finished attempt together with the
// reservation.
func (s *Server) releaseSetupApplyClaim(setupID string) {
	s.releaseFenced(&s.operation.setup, OperationInstall, setupID)
}

// RunSetupApply executes one native setup apply attempt start to finish.
// The caller has already admitted it and runs this in its own goroutine.
// This function owns everything that happens next: bounding the attempt,
// guarding against a panic, emitting every status event, releasing the
// reservation before the start step so the start can claim it, and
// reporting the result to the panel exactly once.
func (s *Server) RunSetupApply(req setupapply.Request) {
	start := time.Now()
	var applyErr error
	released := false

	// release is idempotent so the finisher can call it again on the paths
	// where the pipeline never reached the start step...
	release := func() {
		if !released {
			released = true
			s.releaseSetupApplyClaim(req.SetupID)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			applyErr = errors.New("setupapply: unexpected internal error")
			s.Log().WithField("panic", r).WithField("setup_id", req.SetupID).Error("setup apply: recovered panic")
		}
		release()
		s.finishSetupApply(req, applyErr, start)
	}()

	ctx, cancel := context.WithTimeout(s.Context(), setupApplyTimeout())
	defer cancel()

	applyErr = s.runSetupApplyPipeline(ctx, req, release)
}

// runSetupApplyPipeline drives stop, apply, start in order, publishing a
// status event ahead of each. It returns the first error; the deferred
// finisher reports it.
func (s *Server) runSetupApplyPipeline(ctx context.Context, req setupapply.Request, release func()) error {
	status := func(state string) {
		s.Events().Publish(SetupApplyStatusEvent, SetupApplyStatus{SetupID: req.SetupID, State: state})
	}

	fail := func(code string, err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &stageError{code: SetupApplyErrorTimeout, err: err}
		}
		return &stageError{code: code, err: err}
	}

	interrupted := func() error {
		switch err := ctx.Err(); {
		case err == nil:
			return nil
		case errors.Is(err, context.DeadlineExceeded):
			return &stageError{code: SetupApplyErrorTimeout, err: errors.New("setupapply: timed out")}
		default:
			return &stageError{code: SetupApplyErrorInternal, err: errors.New("setupapply: cancelled")}
		}
	}

	status("stopping")
	if s.Environment.State() != environment.ProcessOfflineState {
		if err := s.Environment.WaitForStop(ctx, setupApplyStopGrace, true); err != nil {
			s.Log().WithField("error", err).WithField("setup_id", req.SetupID).Warn("setup apply: failed to stop the server")
			return fail(SetupApplyErrorStopFailed, errors.New("setupapply: failed to stop the server"))
		}

		// WaitForStop returns as soon as the process is gone, while the
		// transition to the offline state is published a moment later by
		// the environment's own attach stream. A start issued before then
		// is refused as already running, so the job waits it out...
		s.awaitOfflineState(ctx)
	}
	if err := interrupted(); err != nil {
		return err
	}

	status("applying")
	if err := setupapply.Apply(s.Filesystem(), req); err != nil {
		// The underlying error can carry host paths, and every apply error
		// reaches the panel and every websocket viewer verbatim, so the
		// detail stays in the node's own log and the attempt reports only
		// this step's fixed message...
		s.Log().WithField("error", err).WithField("setup_id", req.SetupID).Warn("setup apply: failed to apply the setup files")
		return fail(SetupApplyErrorApplyFailed, errors.New("setupapply: failed to apply the setup files"))
	}
	if err := interrupted(); err != nil {
		return err
	}

	// HandlePowerAction claims the same reservation this job holds, so the
	// job lets go first. A user power action that slips in between is a
	// start the user asked for, which is fine...
	status("starting")
	release()
	if err := s.HandlePowerAction(PowerActionStart, setupApplyStartWait); err != nil {
		// A user power action can win the reservation in the moment between
		// the release above and this call, leaving the server already
		// booting by the time the start step looks. That is the end state
		// this job was asking for, so it is a success, not a failure. The
		// same error is reported for any state that is not offline though,
		// stopping included, so only a server that is genuinely up counts...
		if errors.Is(err, ErrIsRunning) {
			if state := s.Environment.State(); state == environment.ProcessRunningState || state == environment.ProcessStartingState {
				return nil
			}
		}
		s.Log().WithField("error", err).WithField("setup_id", req.SetupID).Warn("setup apply: failed to start the server")
		return fail(SetupApplyErrorStartFailed, errors.New("setupapply: failed to start the server"))
	}
	return nil
}

// awaitOfflineState blocks until the environment reports the server
// offline, or until ctx is done. It is bounded only by the attempt's own
// context, so a stop that never settles ends the attempt as a timeout, or
// as an internal error when the server itself is being torn down, rather
// than under the stop step's own code: the stop command itself succeeded,
// and only the deadline can say how long the job is willing to wait.
func (s *Server) awaitOfflineState(ctx context.Context) {
	for s.Environment.State() != environment.ProcessOfflineState {
		select {
		case <-ctx.Done():
			return
		case <-time.After(setupApplySettleInterval):
		}
	}
}

// finishSetupApply is the single exit path for an attempt: it tells the
// panel the outcome on a fresh context, then publishes the terminal event,
// so consumers learn of an outcome no earlier than the node itself
// considers the attempt finished.
func (s *Server) finishSetupApply(req setupapply.Request, applyErr error, start time.Time) {
	result := remote.SetupApplyResultRequest{
		SetupID:    req.SetupID,
		Successful: applyErr == nil,
		DurationMs: time.Since(start).Milliseconds(),
	}
	terminal := SetupApplyStatus{SetupID: req.SetupID, State: "completed"}
	if applyErr != nil {
		result.Error = applyErr.Error()
		result.ErrorCode = setupApplyErrorCode(applyErr)
		terminal.State = "failed"
		terminal.Error = result.Error
		terminal.ErrorCode = result.ErrorCode
		s.Log().WithField("error", applyErr).
			WithField("error_code", result.ErrorCode).
			WithField("setup_id", req.SetupID).
			Warn("setup apply: attempt failed")
	} else {
		s.Log().WithField("setup_id", req.SetupID).Info("setup apply: attempt finished")
	}

	reportCtx, cancelReport := fencedResultReportContext()
	reportErr := s.client.SendSetupApplyResult(reportCtx, s.ID(), result)
	cancelReport()
	if reportErr != nil {
		s.Log().WithField("error", reportErr).WithField("setup_id", req.SetupID).Warn("setup apply: failed to report result to panel")
	}

	s.Events().Publish(SetupApplyStatusEvent, terminal)
}

// setupApplyErrorCode classifies a finished attempt's error for the panel.
func setupApplyErrorCode(err error) string {
	var stage *stageError
	if errors.As(err, &stage) {
		return stage.code
	}
	return SetupApplyErrorInternal
}
