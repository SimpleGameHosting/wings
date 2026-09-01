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
}
