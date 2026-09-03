package server

import (
	"context"
	"sync"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/modpackinstall"
	"github.com/pterodactyl/wings/remote"
)

// ModpackInstallStatus is the payload of every "modpack install status"
// websocket event. Error carries a sanitized message only, since it is
// relayed straight to the panel UI, and ErrorCode carries the stable
// machine-readable classification of the same failure, one of the
// ModpackInstallError constants below, so the panel can branch on an
// outcome without parsing English out of a message that may be reworded.
// Both are absent from a successful attempt's events.
type ModpackInstallStatus struct {
	InstallID string `json:"install_id"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

// The stable error codes a failed native install reports, on both the
// terminal status event and the panel callback. They name the pipeline
// stage that failed, except for the two that describe how the attempt
// ended rather than where: ModpackInstallErrorTimeout when the attempt's
// own deadline expired, and ModpackInstallErrorInternal for a cancelled
// job or a recovered panic. The set is a contract with the panel and must
// only ever grow.
const (
	ModpackInstallErrorStopFailed     = "stop_failed"
	ModpackInstallErrorSyncFailed     = "sync_failed"
	ModpackInstallErrorCleanFailed    = "clean_failed"
	ModpackInstallErrorDownloadFailed = "download_failed"
	ModpackInstallErrorExtractFailed  = "extract_failed"
	ModpackInstallErrorFinalizeFailed = "finalize_failed"
	ModpackInstallErrorTimeout        = "timeout"
	ModpackInstallErrorInternal       = "internal_error"
)

// stageError pairs a pipeline stage's failure with the stable code that
// classifies it, so the finisher can report both without having to guess
// the stage back out of an error message.
type stageError struct {
	code string
	err  error
}

// Error returns the wrapped failure's message, which is the sanitized text
// the panel and the websocket receive.
func (e *stageError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the underlying failure so errors.Is and errors.As still
// see through the classification wrapper.
func (e *stageError) Unwrap() error {
	return e.err
}

// modpackInstallErrorCode classifies a finished attempt's error for the
// panel. A stage that tagged its own failure decides the code; anything
// else, a recovered panic in particular, is an internal error.
func modpackInstallErrorCode(err error) string {
	var stage *stageError
	if errors.As(err, &stage) {
		return stage.code
	}
	return ModpackInstallErrorInternal
}

// ModpackInstallProgress is the payload of "modpack install progress".
type ModpackInstallProgress struct {
	InstallID string `json:"install_id"`
	State     string `json:"state"`
	Bytes     int64  `json:"bytes"`
	Total     int64  `json:"total"`
}

// TryReserveModpackInstallSlot claims one of the node's concurrent native
// install slots, capped at config.Get().System.ModpackInstall.MaxConcurrent.
// The returned release func gives back the slot and is safe to call more
// than once, so a caller with several deferred or error-path releases can
// never over-release into a negative count.
func (m *Manager) TryReserveModpackInstallSlot() (func(), bool) {
	m.modpackInstallSlots.mu.Lock()
	defer m.modpackInstallSlots.mu.Unlock()

	if m.modpackInstallSlots.active >= config.Get().System.ModpackInstall.MaxConcurrent {
		return nil, false
	}
	m.modpackInstallSlots.active++

	var once sync.Once
	return func() {
		once.Do(func() {
			m.modpackInstallSlots.mu.Lock()
			defer m.modpackInstallSlots.mu.Unlock()
			m.modpackInstallSlots.active--
		})
	}, true
}

// AdmitModpackInstall admits one native install attempt in a single
// critical section: a repeat of the running or most recently finished id
// reports repeat true without claiming, otherwise the install reservation
// is claimed and the id recorded together, or ErrOperationInProgress is
// returned when another operation holds the server.
func (s *Server) AdmitModpackInstall(id string) (bool, error) {
	return s.admitFenced(&s.operation.install, OperationInstall, id)
}

// ActiveModpackInstallID reports the install_id of the native install
// currently running against this server, or the empty string.
func (s *Server) ActiveModpackInstallID() string {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()
	return s.operation.install.active
}

// AbandonModpackInstallClaim releases a claim whose job never started (the
// node slot was full) without remembering the id as finished, so the
// panel's retry of that id is admitted as a fresh attempt.
func (s *Server) AbandonModpackInstallClaim(installID string) {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	if s.operation.install.active == installID {
		s.operation.install.active = ""
	}
	s.endOperationLocked(OperationInstall)
}

// releaseModpackInstallClaim retires a finished attempt together with the
// install reservation.
func (s *Server) releaseModpackInstallClaim(installID string) {
	s.releaseFenced(&s.operation.install, OperationInstall, installID)
}

// RunModpackInstall executes one native modpack/version install attempt
// start to finish. The caller (the install router) has already claimed the
// operation reservation and a node slot before calling this and runs it in
// its own goroutine; this function owns everything that happens next:
// bounding the attempt with a timeout, guarding against a panic anywhere in
// the pipeline, emitting every status and progress event, and, no matter
// how the attempt ends, releasing the reservation and slot and reporting
// the result back to the panel exactly once.
func (s *Server) RunModpackInstall(req modpackinstall.Request, release func()) {
	start := time.Now()
	var installErr error

	// This deferred func is registered before anything else can fail so it
	// is the single exit point for the attempt: it runs whether the
	// pipeline below returns a plain error, returns nil, or panics, so the
	// finisher underneath it is guaranteed to run exactly once regardless
	// of how the attempt ends...
	defer func() {
		if r := recover(); r != nil {
			installErr = errors.New("modpackinstall: unexpected internal error")
			s.Log().WithField("panic", r).WithField("install_id", req.InstallID).Error("modpack install: recovered panic")
		}
		s.finishModpackInstall(req, installErr, start, release)
	}()

	timeout := time.Duration(config.Get().System.ModpackInstall.TimeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(s.Context(), timeout)
	defer cancel()

	installErr = s.runModpackInstallPipeline(ctx, req)
}

// runModpackInstallPipeline drives the ordered stages of a single install
// attempt, publishing a status event ahead of each one. It returns the
// first error encountered; the caller's deferred finisher reports it, so
// this function itself never needs to release anything or contact the
// panel.
func (s *Server) runModpackInstallPipeline(ctx context.Context, req modpackinstall.Request) error {
	status := func(state string) {
		s.Events().Publish(ModpackInstallStatusEvent, ModpackInstallStatus{InstallID: req.InstallID, State: state})
	}

	// fail classifies a stage's failure, upgrading it to the timeout code
	// when the attempt's own deadline is what actually stopped it rather
	// than anything about the stage itself...
	fail := func(code string, err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &stageError{code: ModpackInstallErrorTimeout, err: err}
		}
		return &stageError{code: code, err: err}
	}

	// interrupted ends the attempt between two stages once the job context
	// is done, so a daemon shutdown or a server deleted mid-install stops
	// promptly instead of running the rest of the pipeline against a
	// server nobody is waiting for any more...
	interrupted := func() error {
		switch err := ctx.Err(); {
		case err == nil:
			return nil
		case errors.Is(err, context.DeadlineExceeded):
			return &stageError{code: ModpackInstallErrorTimeout, err: errors.New("modpackinstall: timed out")}
		default:
			return &stageError{code: ModpackInstallErrorInternal, err: errors.New("modpackinstall: cancelled")}
		}
	}

	status("stopping")
	if err := s.Environment.WaitForStop(ctx, 10*time.Second, true); err != nil {
		return fail(ModpackInstallErrorStopFailed, errors.New("modpackinstall: failed to stop the server"))
	}
	if err := interrupted(); err != nil {
		return err
	}

	status("syncing")
	if err := s.Sync(); err != nil {
		return fail(ModpackInstallErrorSyncFailed, errors.New("modpackinstall: failed to sync server configuration"))
	}
	if err := interrupted(); err != nil {
		return err
	}

	status("cleaning")
	if err := modpackinstall.Clean(s.Filesystem(), req.Kind); err != nil {
		// Unlike the other modpackinstall calls below, Clean returns bare
		// filesystem errors with no stage context of their own, so wrap
		// here rather than propagating it unlabeled; nothing about the
		// cause is secret, since Clean never touches the network.
		return fail(ModpackInstallErrorCleanFailed, errors.Wrap(err, "modpackinstall: failed to clean the server directory"))
	}
	if err := interrupted(); err != nil {
		return err
	}

	status("downloading")
	progress := func(bytes, total int64) {
		s.Events().Publish(ModpackInstallProgressEvent, ModpackInstallProgress{
			InstallID: req.InstallID, State: "downloading", Bytes: bytes, Total: total,
		})
	}
	if _, err := modpackinstall.Download(ctx, s.Filesystem(), req.DownloadURL, progress); err != nil {
		// Download errors are already sanitized of the signed URL.
		return fail(ModpackInstallErrorDownloadFailed, err)
	}
	if err := interrupted(); err != nil {
		return err
	}

	// A raw jar artifact is placed directly under its runtime name; anything
	// else is an archive that must be extracted and settled into place. Both
	// are the same step to the panel, so both report extract_failed...
	if req.ArchiveFormat == modpackinstall.FormatJar {
		if err := modpackinstall.PlaceJar(s.Filesystem(), "server.jar"); err != nil {
			return fail(ModpackInstallErrorExtractFailed, err)
		}
	} else {
		status("extracting")
		if err := modpackinstall.ExtractToStaging(ctx, s.Filesystem()); err != nil {
			return fail(ModpackInstallErrorExtractFailed, err)
		}
		if err := modpackinstall.Settle(s.Filesystem()); err != nil {
			return fail(ModpackInstallErrorExtractFailed, err)
		}
	}
	if err := interrupted(); err != nil {
		return err
	}

	status("finalizing")
	if err := modpackinstall.Finalize(s.Filesystem(), req.Kind, req.VersionType); err != nil {
		return fail(ModpackInstallErrorFinalizeFailed, err)
	}
	return nil
}

// finishModpackInstall is the single exit path for a native install attempt,
// reached exactly once whether it succeeded, failed, or panicked. The order
// below is deliberate: exclusivity is released first (retiring the install
// identity together with the operation reservation, then freeing the node
// slot) so a retried request is never blocked by bookkeeping that has
// already served its purpose, then the panel is told the outcome on a
// fresh context so a cancelled or expired job context can never suppress
// the callback, and only then is the terminal event published, since panel
// and websocket consumers should learn of an outcome no earlier than the
// node itself considers the attempt finished.
func (s *Server) finishModpackInstall(req modpackinstall.Request, installErr error, start time.Time, release func()) {
	s.releaseModpackInstallClaim(req.InstallID)
	release()

	result := remote.ModpackInstallResultRequest{
		InstallID:  req.InstallID,
		Successful: installErr == nil,
		DurationMs: time.Since(start).Milliseconds(),
	}
	terminal := ModpackInstallStatus{InstallID: req.InstallID, State: "completed"}
	if installErr != nil {
		result.Error = installErr.Error()
		result.ErrorCode = modpackInstallErrorCode(installErr)
		terminal.State = "failed"
		terminal.Error = result.Error
		terminal.ErrorCode = result.ErrorCode
		s.Log().WithField("error", installErr).
			WithField("error_code", result.ErrorCode).
			WithField("install_id", req.InstallID).
			Warn("modpack install: attempt failed")
	}

	if err := s.client.SendModpackInstallResult(context.Background(), s.ID(), result); err != nil {
		s.Log().WithField("error", err).WithField("install_id", req.InstallID).Warn("modpack install: failed to report result to panel")
	}

	s.Events().Publish(ModpackInstallStatusEvent, terminal)
}
