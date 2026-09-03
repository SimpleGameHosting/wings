package server

import (
	"sync"

	"emperror.dev/errors"
)

// Operation names one of the mutually exclusive long-running things a server
// can be doing at once, such as installing, transferring, or restoring. The
// empty string is never a valid Operation: operationLock represents "nothing
// is claimed" as that same empty string internally, so TryBeginOperation and
// EndOperation both reject it, along with any other value outside the four
// constants below, via IsValid.
type Operation string

const (
	OperationInstall  Operation = "install"
	OperationTransfer Operation = "transfer"
	OperationRestore  Operation = "restore"
	OperationPower    Operation = "power"
)

// IsValid reports whether kind is one of the four known operation kinds.
func (o Operation) IsValid() bool {
	return o == OperationInstall ||
		o == OperationTransfer ||
		o == OperationRestore ||
		o == OperationPower
}

// ErrOperationInProgress indicates a server already has an exclusive
// operation in progress. TryBeginOperation wraps it with the name of the
// operation currently holding the reservation.
var ErrOperationInProgress = errors.Sentinel("server: another exclusive operation is in progress")

// ErrUnknownOperation indicates TryBeginOperation was called with an
// Operation value that is not one of the four known kinds, including the
// empty-string zero value. Rejecting it is what stops that zero value from
// being mistaken for "nothing claimed" and silently defeating mutual
// exclusion.
var ErrUnknownOperation = errors.Sentinel("server: unknown operation kind")

// operationLock serializes claims across the mutually exclusive operation
// kinds a server can run. The legacy AtomicBool flags (installing,
// transferring, restoring) remain the externally visible state that the rest
// of the daemon reads; this mutex is what makes claiming them race-free by
// turning "check nothing is running, then set mine" into one atomic step.
type operationLock struct {
	mu      sync.Mutex
	current Operation

	// install and setup hold the identity of the fenced job of each kind
	// this server has admitted, so a retried request can be told apart
	// from a genuinely new one. They live under this same mutex, rather
	// than one of their own, so the identity and the reservation are
	// always claimed and released as a single indivisible step.
	install fencedIdentity
	setup   fencedIdentity
}

// fencedIdentity remembers the running attempt of one fenced job kind and
// the one that finished most recently. Both answer the panel's question
// the same way: this attempt has already been admitted once, so a repeat
// of it is a retry of a lost 202 and must never start a second job.
type fencedIdentity struct {
	active       string
	lastFinished string
}

// isRecent reports whether id names the running or the last finished attempt.
func (f *fencedIdentity) isRecent(id string) bool {
	return id != "" && (id == f.active || id == f.lastFinished)
}

// admitFenced is the one critical section every fenced job admits through:
// a repeat of a recent id is answered without claiming, otherwise the
// reservation is claimed for kind and the id recorded, in one hold of the
// mutex. Two simultaneous requests for the same id therefore both learn it
// is admitted and exactly one job runs. Callers must not hold s.operation.mu.
//
// Like TryBeginOperation, it must never be called while the caller already
// holds the Server's own mutex (s.Lock/s.RLock): mirroring the legacy flag
// goes through SetInstalling and friends, which acquire that same mutex.
func (s *Server) admitFenced(identity *fencedIdentity, kind Operation, id string) (bool, error) {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	if identity.isRecent(id) {
		return true, nil
	}
	if s.operation.current != "" {
		return false, errors.Wrapf(ErrOperationInProgress, "current operation: %s", s.operation.current)
	}

	s.operation.current = kind
	s.setLegacyFlag(kind, true)
	identity.active = id

	return false, nil
}

// releaseFenced retires a finished attempt: it remembers the id as the last
// one to finish, clears the active id, and releases the reservation, all
// inside one hold of the mutex.
func (s *Server) releaseFenced(identity *fencedIdentity, kind Operation, id string) {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	identity.lastFinished = id
	identity.active = ""
	s.endOperationLocked(kind)
}

// TryBeginOperation atomically claims exclusive ownership of the server for
// the given operation kind, setting the matching legacy flag while the lock
// is held so no other kind can slip in between the check and the set. It
// returns ErrUnknownOperation if kind is not one of the four known kinds
// (this also rejects the empty-string zero value, which would otherwise be
// mistaken for "nothing claimed"), and ErrOperationInProgress, naming the
// current holder, when another operation already owns the reservation.
//
// TryBeginOperation must never be called while the caller already holds the
// Server's own mutex (s.Lock/s.RLock). Claiming install, transfer, or
// restore mirrors the legacy flag through SetInstalling, SetTransferring, or
// SetRestoring, which calls Sftp() and so acquires that same mutex; calling
// in from inside it would deadlock permanently.
func (s *Server) TryBeginOperation(kind Operation) error {
	if !kind.IsValid() {
		return errors.Wrapf(ErrUnknownOperation, "kind: %q", kind)
	}

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
// legacy flag. Releasing an unknown kind (including the empty string), or a
// kind that does not currently hold the reservation, is a no-op, so a stale,
// duplicate, or malformed release can never clear another operation's claim.
func (s *Server) EndOperation(kind Operation) {
	if !kind.IsValid() {
		return
	}

	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	s.endOperationLocked(kind)
}

// endOperationLocked is EndOperation's body, split out so a caller that
// already holds the reservation mutex can release the claim and its own
// bookkeeping in one indivisible step. Callers must hold s.operation.mu.
func (s *Server) endOperationLocked(kind Operation) {
	if s.operation.current != kind {
		return
	}

	s.operation.current = ""
	s.setLegacyFlag(kind, false)
}

// CurrentOperation returns the operation kind currently holding the shared
// reservation, or the empty string if nothing is claimed. It exists for
// building a user-facing message after a failed TryBeginOperation call; like
// the pre-existing IsInstalling()-style flag reads elsewhere in this
// package, the value can go stale immediately after being read if another
// caller claims or releases concurrently.
func (s *Server) CurrentOperation() Operation {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()

	return s.operation.current
}

// setLegacyFlag mirrors the reservation into the pre-existing AtomicBool
// flags that the rest of the daemon (and the panel) observe, so every
// existing IsInstalling()-style consumer keeps working unchanged. Both
// callers above validate kind with IsValid() before reaching here, so the
// default case is unreachable in practice; it stays a documented no-op
// rather than a panic so that a future Operation constant added without a
// matching case here fails safe, by mirroring nothing, instead of crashing
// the daemon.
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
	default:
		// Unreachable: both call sites guard with kind.IsValid() first.
	}
}
