package server

import (
	"context"
	"testing"
	"time"

	. "github.com/franela/goblin"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/system"
)

func TestPower(t *testing.T) {
	g := Goblin(t)

	g.Describe("Server#ExecutingPowerAction", func() {
		g.It("should return based on locker status", func() {
			s := &Server{powerLock: system.NewLocker()}

			g.Assert(s.ExecutingPowerAction()).IsFalse()
			s.powerLock.Acquire()
			g.Assert(s.ExecutingPowerAction()).IsTrue()
		})
	})
}

// TestClaimPowerOperationFailureReleasesOnlyThePowerLock pins the fix for a
// reservation-stomping race: when a power action has acquired the power lock
// but a different, still-active power action already holds the shared
// operation reservation, losing that race must release only the power lock
// this call itself just acquired, never the reservation, which belongs to
// that other caller.
//
// Before the fix, the failure branch called the same cleanup used on a
// successful run, which released the power lock and then unconditionally
// cleared the reservation by kind. Because the reservation is tracked only
// by kind, not by which caller claimed it, that clear silently ended the
// other caller's still-running power action out from under it - including a
// kill that had raced in to grab the lock right as the first caller released
// it, which would then itself return an error instead of reaching
// Environment.Terminate.
func TestClaimPowerOperationFailureReleasesOnlyThePowerLock(t *testing.T) {
	s, err := New(nil)
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)

	// Simulate a first power action that has already claimed the shared
	// reservation, exactly as HandlePowerAction does once it acquires the
	// power lock...
	require.NoError(t, s.TryBeginOperation(OperationPower))

	// Simulate a second power action that has independently acquired the
	// power lock and then loses the race for the shared reservation, which
	// the first action still legitimately holds...
	require.NoError(t, s.powerLock.Acquire())
	require.Error(t, s.claimPowerOperation())

	// The first action's reservation must still be intact: a third caller of
	// any kind must still be rejected...
	require.Error(t, s.TryBeginOperation(OperationInstall))

	// The power lock the second call acquired must have been released, so a
	// legitimate third caller is not left stuck waiting on a lock nobody is
	// actually using...
	require.NoError(t, s.powerLock.Acquire())
	s.powerLock.Release()

	s.EndOperation(OperationPower)
}

// blockingPowerActionEnvironment lets a test hold HandlePowerAction's power
// lock open on demand, and records whether Terminate was actually invoked,
// so a test can assert on the real environment side effect rather than only
// on HandlePowerAction's return value.
type blockingPowerActionEnvironment struct {
	environment.ProcessEnvironment

	release    chan struct{}
	terminated chan struct{}
}

// WaitForStop blocks until the test releases it, keeping the power lock (and,
// once claimed, the operation reservation) held.
func (e *blockingPowerActionEnvironment) WaitForStop(ctx context.Context, _ time.Duration, _ bool) error {
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil
}

// Terminate records that it was actually reached, rather than just returning
// success.
func (e *blockingPowerActionEnvironment) Terminate(_ context.Context, _ string) error {
	close(e.terminated)
	return nil
}

// TestHandlePowerActionTerminateBypassesBusyLock pins the terminate escape
// hatch at the one place that actually matters: it must reach
// Environment.Terminate, not merely return without error. A stop or restart
// that is mid-flight can hold the power lock for a long time (WaitForStop
// waits up to ten minutes); a stuck server's only way out is a kill that
// bypasses that lock entirely.
func TestHandlePowerActionTerminateBypassesBusyLock(t *testing.T) {
	s, err := New(nil)
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)

	release := make(chan struct{})
	terminated := make(chan struct{})
	s.Environment = &blockingPowerActionEnvironment{release: release, terminated: terminated}
	t.Cleanup(func() { close(release) })

	// Occupy the power lock the same way a slow stop or boot would...
	go func() { _ = s.HandlePowerAction(PowerActionStop) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.ExecutingPowerAction() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the fake stop action to hold the power lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	require.NoError(t, s.HandlePowerAction(PowerActionTerminate))

	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("expected Environment.Terminate to have been invoked despite the busy power lock")
	}
}
