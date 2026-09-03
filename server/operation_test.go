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

// TestReleasingAFencedClaimRequiresOwningTheID proves a release naming any
// id other than the one holding the fence leaves the running job's
// reservation and active id untouched. Both fence kinds claim
// OperationInstall, so without that ownership check a stale, duplicated,
// or late release of either kind would tear the server out from under a
// job that is still running.
func TestReleasingAFencedClaimRequiresOwningTheID(t *testing.T) {
	s := newOperationTestServer(t)
	const active = "55555555-5555-5555-5555-555555555555"
	const stranger = "66666666-6666-6666-6666-666666666666"

	if repeat, err := s.AdmitModpackInstall(active); err != nil || repeat {
		t.Fatalf("first admit must claim, got repeat=%v err=%v", repeat, err)
	}

	// Neither a stale release from the other fence nor one from this fence
	// may disturb the claim, since neither names the id that owns it...
	s.releaseSetupApplyClaim(stranger)
	s.releaseModpackInstallClaim(stranger)
	s.AbandonModpackInstallClaim(stranger)

	if current := s.CurrentOperation(); current != OperationInstall {
		t.Fatalf("a release for a foreign id cleared the reservation, current = %q", current)
	}
	if id := s.ActiveModpackInstallID(); id != active {
		t.Fatalf("a release for a foreign id cleared the active id, got %q want %q", id, active)
	}
	if !s.IsInstalling() {
		t.Fatal("a release for a foreign id cleared the legacy installing flag")
	}

	s.releaseModpackInstallClaim(active)

	if current := s.CurrentOperation(); current != "" {
		t.Fatalf("the owning release must clear the reservation, got %q", current)
	}
	if id := s.ActiveModpackInstallID(); id != "" {
		t.Fatalf("the owning release must clear the active id, got %q", id)
	}
}

// TestAbandonModpackInstallClaimForgetsTheAttempt pins the abandon rule: a
// claim whose job never started releases everything but is deliberately
// not remembered as finished, so the panel's retry of that same id is
// admitted as a fresh attempt rather than answered as a repeat that never
// ran.
func TestAbandonModpackInstallClaimForgetsTheAttempt(t *testing.T) {
	s := newOperationTestServer(t)
	const id = "77777777-7777-7777-7777-777777777777"

	if repeat, err := s.AdmitModpackInstall(id); err != nil || repeat {
		t.Fatalf("first admit must claim, got repeat=%v err=%v", repeat, err)
	}

	s.AbandonModpackInstallClaim(id)

	if current := s.CurrentOperation(); current != "" {
		t.Fatalf("abandon must release the reservation, got %q", current)
	}
	if active := s.ActiveModpackInstallID(); active != "" {
		t.Fatalf("abandon must clear the active id, got %q", active)
	}
	if repeat, err := s.AdmitModpackInstall(id); err != nil || repeat {
		t.Fatalf("an abandoned id must be admitted afresh, got repeat=%v err=%v", repeat, err)
	}
}

// TestAdmitFencedRefusesAnEmptyID proves an identity-less admission claims
// nothing. The empty id is what releaseFenced treats as owning nothing, so
// admitting one would leave the reservation held with no release able to
// give it back.
func TestAdmitFencedRefusesAnEmptyID(t *testing.T) {
	s := newOperationTestServer(t)

	repeat, err := s.AdmitSetupApply("")
	assert.False(t, repeat)
	assert.True(t, errors.Is(err, ErrEmptyFencedID))
	assert.Equal(t, Operation(""), s.CurrentOperation())
	assert.False(t, s.IsInstalling())

	// The server must still be free for a real attempt afterwards...
	admitted, err := s.AdmitSetupApply("88888888-8888-8888-8888-888888888888")
	require.NoError(t, err)
	assert.False(t, admitted)
}

// TestAdmitFencedRefusesAnUnknownKind proves the kind guard TryBeginOperation
// applies is applied here too, so the empty-string zero value can never be
// mistaken for "nothing claimed" and silently defeat mutual exclusion.
func TestAdmitFencedRefusesAnUnknownKind(t *testing.T) {
	s := newOperationTestServer(t)

	repeat, err := s.admitFenced(&s.operation.setup, Operation("nonsense"), "99999999-9999-9999-9999-999999999999")
	assert.False(t, repeat)
	assert.True(t, errors.Is(err, ErrUnknownOperation))
	assert.Equal(t, Operation(""), s.CurrentOperation())

	repeat, err = s.admitFenced(&s.operation.setup, Operation(""), "99999999-9999-9999-9999-999999999999")
	assert.False(t, repeat)
	assert.True(t, errors.Is(err, ErrUnknownOperation))
	assert.Equal(t, Operation(""), s.CurrentOperation())
}
