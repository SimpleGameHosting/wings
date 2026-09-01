package server

import (
	"sync"

	"emperror.dev/errors"
)

// Operation names one of the mutually exclusive long-running things a server
// can be doing at once, such as installing, transferring, or restoring.
type Operation string

const (
	OperationInstall  Operation = "install"
	OperationTransfer Operation = "transfer"
	OperationRestore  Operation = "restore"
	OperationPower    Operation = "power"
)

// ErrOperationInProgress indicates a server already has an exclusive
// operation in progress. TryBeginOperation wraps it with the name of the
// operation currently holding the reservation.
var ErrOperationInProgress = errors.Sentinel("server: another exclusive operation is in progress")

// operationLock serializes claims across the mutually exclusive operation
// kinds a server can run. The legacy AtomicBool flags (installing,
// transferring, restoring) remain the externally visible state that the rest
// of the daemon reads; this mutex is what makes claiming them race-free by
// turning "check nothing is running, then set mine" into one atomic step.
type operationLock struct {
	mu      sync.Mutex
	current Operation
}

// TryBeginOperation atomically claims exclusive ownership of the server for
// the given operation kind, setting the matching legacy flag while the lock
// is held so no other kind can slip in between the check and the set. It
// returns ErrOperationInProgress, naming the current holder, when another
// operation already owns the reservation.
func (s *Server) TryBeginOperation(kind Operation) error {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	if s.operation.current != "" {
		return errors.Wrapf(ErrOperationInProgress, "current operation: %s", s.operation.current)
	}

	s.operation.current = kind
	s.setLegacyFlag(kind, true)

	return nil
}

// EndOperation releases the reservation held for kind, clearing its matching
// legacy flag. Releasing a kind that does not currently hold the reservation
// is a no-op, so a stale or duplicate release can never clear another
// operation's claim.
func (s *Server) EndOperation(kind Operation) {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	if s.operation.current != kind {
		return
	}

	s.operation.current = ""
	s.setLegacyFlag(kind, false)
}

// setLegacyFlag mirrors the reservation into the pre-existing AtomicBool
// flags that the rest of the daemon (and the panel) observe, so every
// existing IsInstalling()-style consumer keeps working unchanged.
func (s *Server) setLegacyFlag(kind Operation, state bool) {
	switch kind {
	case OperationInstall:
		s.SetInstalling(state)
	case OperationTransfer:
		s.SetTransferring(state)
	case OperationRestore:
		s.SetRestoring(state)
	case OperationPower:
		// Power has no legacy AtomicBool flag; the pre-existing power lock
		// is already its own visible state and is left untouched here.
	}
}
