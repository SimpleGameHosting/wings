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
// relayed straight to the panel UI.
type ModpackInstallStatus struct {
	InstallID string `json:"install_id"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
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

// ActiveModpackInstallID reports the install_id of the native install
// currently running against this server, or the empty string when none is
// running. The router reads this to tell a retried request apart from a
// genuinely new one.
func (s *Server) ActiveModpackInstallID() string {
	s.modpackInstall.mu.Lock()
	defer s.modpackInstall.mu.Unlock()
	return s.modpackInstall.activeID
}

// setActiveModpackInstallID records or clears the install_id of the native
// install currently running against this server.
func (s *Server) setActiveModpackInstallID(id string) {
	s.modpackInstall.mu.Lock()
	defer s.modpackInstall.mu.Unlock()
	s.modpackInstall.activeID = id
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
	s.setActiveModpackInstallID(req.InstallID)

	timeout := time.Duration(config.Get().System.ModpackInstall.TimeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(s.Context(), timeout)
	defer cancel()

	var installErr error
	// This deferred func is the single exit point for the attempt: it runs
	// whether the pipeline below returns a plain error, returns nil, or
	// panics, so the finisher underneath it is guaranteed to run exactly
	// once regardless of how the attempt ends...
	defer func() {
		if r := recover(); r != nil {
			installErr = errors.New("modpackinstall: unexpected internal error")
			s.Log().WithField("panic", r).WithField("install_id", req.InstallID).Error("modpack install: recovered panic")
		}
		s.finishModpackInstall(req, installErr, start, release)
	}()

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

	status("stopping")
	if err := s.Environment.WaitForStop(ctx, 10*time.Second, true); err != nil {
		return errors.New("modpackinstall: failed to stop the server")
	}

	status("syncing")
	if err := s.Sync(); err != nil {
		return errors.New("modpackinstall: failed to sync server configuration")
	}

	status("cleaning")
	if err := modpackinstall.Clean(s.Filesystem(), req.Kind); err != nil {
		// Unlike the other modpackinstall calls below, Clean returns bare
		// filesystem errors with no stage context of their own, so wrap
		// here rather than propagating it unlabeled; nothing about the
		// cause is secret, since Clean never touches the network.
		return errors.Wrap(err, "modpackinstall: failed to clean the server directory")
	}

	status("downloading")
	progress := func(bytes, total int64) {
		s.Events().Publish(ModpackInstallProgressEvent, ModpackInstallProgress{
			InstallID: req.InstallID, State: "downloading", Bytes: bytes, Total: total,
		})
	}
	if _, err := modpackinstall.Download(ctx, s.Filesystem(), req.DownloadURL, progress); err != nil {
		return err // download errors are already sanitized of the signed URL
	}

	// A raw jar artifact is placed directly under its runtime name; anything
	// else is an archive that must be extracted and settled into place...
	if req.ArchiveFormat == modpackinstall.FormatJar {
		if err := modpackinstall.PlaceJar(s.Filesystem(), "server.jar"); err != nil {
			return err
		}
	} else {
		status("extracting")
		if err := modpackinstall.ExtractToStaging(ctx, s.Filesystem()); err != nil {
			return err
		}
		if err := modpackinstall.Settle(s.Filesystem()); err != nil {
			return err
		}
	}

	status("finalizing")
	return modpackinstall.Finalize(s.Filesystem(), req.Kind, req.VersionType)
}

// finishModpackInstall is the single exit path for a native install attempt,
// reached exactly once whether it succeeded, failed, or panicked. The order
// below is deliberate: exclusivity is released first (clearing the active
// id, ending the operation reservation, and freeing the node slot) so a
// retried request is never blocked by bookkeeping that has already served
// its purpose, then the panel is told the outcome on a fresh context so a
// cancelled or expired job context can never suppress the callback, and
// only then is the terminal event published, since panel and websocket
// consumers should learn of an outcome no earlier than the node itself
// considers the attempt finished.
func (s *Server) finishModpackInstall(req modpackinstall.Request, installErr error, start time.Time, release func()) {
	s.setActiveModpackInstallID("")
	s.EndOperation(OperationInstall)
	release()

	result := remote.ModpackInstallResultRequest{
		InstallID:  req.InstallID,
		Successful: installErr == nil,
		DurationMs: time.Since(start).Milliseconds(),
	}
	terminal := ModpackInstallStatus{InstallID: req.InstallID, State: "completed"}
	if installErr != nil {
		result.Error = installErr.Error()
		terminal.State = "failed"
		terminal.Error = installErr.Error()
		s.Log().WithField("error", installErr).WithField("install_id", req.InstallID).Warn("modpack install: attempt failed")
	}

	if err := s.client.SendModpackInstallResult(context.Background(), s.ID(), result); err != nil {
		s.Log().WithField("error", err).WithField("install_id", req.InstallID).Warn("modpack install: failed to report result to panel")
	}

	s.Events().Publish(ModpackInstallStatusEvent, terminal)
}
