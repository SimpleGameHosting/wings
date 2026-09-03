package server

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOperationTestServer builds a fully initialized Server, since the legacy
// flag setters that TryBeginOperation drives dereference the AtomicBool
// pointers and SFTP context bag that only New() wires up.
func newOperationTestServer(t *testing.T) *Server {
	t.Helper()

	s, err := New(nil)
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)

	return s
}

// TestOperationReservation locks down TryBeginOperation and EndOperation as
// the single race-free gate for the mutually exclusive install/transfer/
// restore/power operations, including the legacy AtomicBool flags they still
// drive for existing IsInstalling()-style consumers.
func TestOperationReservation(t *testing.T) {
	t.Run("claims and releases a single operation", func(t *testing.T) {
		s := newOperationTestServer(t)

		require.NoError(t, s.TryBeginOperation(OperationInstall))
		assert.True(t, s.IsInstalling())

		err := s.TryBeginOperation(OperationRestore)
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrOperationInProgress))

		s.EndOperation(OperationInstall)
		assert.False(t, s.IsInstalling())

		assert.NoError(t, s.TryBeginOperation(OperationRestore))
		s.EndOperation(OperationRestore)
	})

	t.Run("EndOperation with the wrong kind is a no-op", func(t *testing.T) {
		s := newOperationTestServer(t)

		require.NoError(t, s.TryBeginOperation(OperationTransfer))
		s.EndOperation(OperationPower)
		assert.True(t, s.IsTransferring())

		s.EndOperation(OperationTransfer)
	})

	t.Run("never double-claims under concurrency", func(t *testing.T) {
		s := newOperationTestServer(t)

		kinds := []Operation{OperationInstall, OperationTransfer, OperationRestore, OperationPower}
		var wins int64
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				if err := s.TryBeginOperation(kinds[n%len(kinds)]); err == nil {
					atomic.AddInt64(&wins, 1)
				}
			}(i)
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins)
	})

	t.Run("power claims exclusivity without setting any legacy flag", func(t *testing.T) {
		s := newOperationTestServer(t)

		require.NoError(t, s.TryBeginOperation(OperationPower))
		assert.False(t, s.IsInstalling())
		assert.False(t, s.IsTransferring())
		assert.False(t, s.IsRestoring())

		err := s.TryBeginOperation(OperationInstall)
		assert.Error(t, err)

		s.EndOperation(OperationPower)

		// The release must be clean: a different kind can claim the server
		// again right away.
		assert.NoError(t, s.TryBeginOperation(OperationTransfer))
		s.EndOperation(OperationTransfer)
	})

	t.Run("rejects unknown operation kinds and claims nothing", func(t *testing.T) {
		s := newOperationTestServer(t)

		err := s.TryBeginOperation(Operation(""))
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnknownOperation))

		err = s.TryBeginOperation(Operation("garbage"))
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnknownOperation))

		assert.False(t, s.IsInstalling())
		assert.False(t, s.IsTransferring())
		assert.False(t, s.IsRestoring())

		// Neither rejected call reserved anything: a real operation kind can
		// still claim the server afterward.
		require.NoError(t, s.TryBeginOperation(OperationInstall))
		s.EndOperation(OperationInstall)
	})

	t.Run("power claim blocks a concurrent install claim and vice versa", func(t *testing.T) {
		s := newOperationTestServer(t)

		require.NoError(t, s.TryBeginOperation(OperationPower))
		assert.Error(t, s.TryBeginOperation(OperationInstall))

		s.EndOperation(OperationPower)

		require.NoError(t, s.TryBeginOperation(OperationInstall))
		s.EndOperation(OperationInstall)
	})

	t.Run("EndOperation with an unknown kind is a no-op", func(t *testing.T) {
		s := newOperationTestServer(t)

		// Neither call may match the unclaimed zero value or reach the
		// legacy-flag switch.
		s.EndOperation(Operation(""))
		s.EndOperation(Operation("garbage"))

		require.NoError(t, s.TryBeginOperation(OperationInstall))
		s.EndOperation(OperationInstall)
	})
}

// TestAdmitModpackInstallIsOneCriticalSection fires many concurrent admits
// of the same id at an idle server and requires exactly one of them to
// claim while every other is answered as a repeat, never as a conflict.
func TestAdmitModpackInstallIsOneCriticalSection(t *testing.T) {
	s := newOperationTestServer(t)
	const id = "11111111-1111-1111-1111-111111111111"

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed, repeats, conflicts := 0, 0, 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repeat, err := s.AdmitModpackInstall(id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				conflicts++
			case repeat:
				repeats++
			default:
				claimed++
			}
		}()
	}
	wg.Wait()

	if claimed != 1 || repeats != 31 || conflicts != 0 {
		t.Fatalf("claimed=%d repeats=%d conflicts=%d, want 1/31/0", claimed, repeats, conflicts)
	}
	if s.ActiveModpackInstallID() != id {
		t.Fatalf("active id = %q, want %q", s.ActiveModpackInstallID(), id)
	}

	// A different id while the first is running is a conflict, and after the
	// claim is released the first id is still a repeat but a new one claims...
	if _, err := s.AdmitModpackInstall("22222222-2222-2222-2222-222222222222"); err == nil {
		t.Fatal("expected a different id to conflict while one is active")
	}
	s.releaseModpackInstallClaim(id)
	if repeat, err := s.AdmitModpackInstall(id); err != nil || !repeat {
		t.Fatalf("finished id must still be a repeat, got repeat=%v err=%v", repeat, err)
	}
	if repeat, err := s.AdmitModpackInstall("22222222-2222-2222-2222-222222222222"); err != nil || repeat {
		t.Fatalf("new id must claim after release, got repeat=%v err=%v", repeat, err)
	}
}

// TestAdmitSetupApplyMirrorsInstallAdmission proves the setup fence has the
// same semantics and shares the reservation with install.
func TestAdmitSetupApplyMirrorsInstallAdmission(t *testing.T) {
	s := newOperationTestServer(t)
	const id = "33333333-3333-3333-3333-333333333333"

	if repeat, err := s.AdmitSetupApply(id); err != nil || repeat {
		t.Fatalf("first admit must claim, got repeat=%v err=%v", repeat, err)
	}
	if s.ActiveSetupApplyID() != id {
		t.Fatalf("active setup id = %q", s.ActiveSetupApplyID())
	}
	if _, err := s.AdmitModpackInstall("44444444-4444-4444-4444-444444444444"); err == nil {
		t.Fatal("an install must conflict while a setup apply holds the server")
	}
	if repeat, err := s.AdmitSetupApply(id); err != nil || !repeat {
		t.Fatalf("same id must be a repeat, got repeat=%v err=%v", repeat, err)
	}
	s.releaseSetupApplyClaim(id)
	if s.ActiveSetupApplyID() != "" || s.CurrentOperation() != "" {
		t.Fatal("release must clear the id and the reservation together")
	}
	if repeat, err := s.AdmitSetupApply(id); err != nil || !repeat {
		t.Fatalf("finished id must still be a repeat, got repeat=%v err=%v", repeat, err)
	}
}
