package server

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/server/filesystem"
	"github.com/pterodactyl/wings/system"
)

// newResourceTestServer builds the smallest Server that Proc() can run against:
// a tracked process state and a filesystem to read the cached disk usage from.
func newResourceTestServer(t *testing.T) *Server {
	t.Helper()
	setNodeConfig(t, false)

	fs, err := filesystem.New(filepath.Join(t.TempDir(), "server"), 0, nil)
	require.NoError(t, err)

	s := &Server{fs: fs}
	s.resources.State = system.NewAtomicString(environment.ProcessOfflineState)
	return s
}

// TestServer_ProcReturnsSnapshot guards the contract every caller relies on:
// Proc() hands back an independent copy of the live usage, and that copy
// marshals to exactly the stats payload the Panel and websocket clients read.
func TestServer_ProcReturnsSnapshot(t *testing.T) {
	s := newResourceTestServer(t)
	s.resources.UpdateStats(environment.Stats{
		Memory:      1024,
		MemoryLimit: 4096,
		CpuAbsolute: 12.5,
		Network:     environment.NetworkStats{RxBytes: 10, TxBytes: 20},
		Uptime:      90500,
	})
	s.resources.State.Store(environment.ProcessRunningState)

	snapshot := s.Proc()

	// Later updates to the live usage must not leak into the snapshot.
	s.resources.UpdateStats(environment.Stats{Memory: 1})
	s.resources.Reset()
	assert.EqualValues(t, 1024, snapshot.Memory)
	assert.EqualValues(t, 90500, snapshot.Uptime)
	assert.Zero(t, s.Proc().Memory)
	assert.Zero(t, s.Proc().Uptime)

	out, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"memory_bytes": 1024,
		"memory_limit_bytes": 4096,
		"cpu_absolute": 12.5,
		"network": {"rx_bytes": 10, "tx_bytes": 20},
		"uptime": 90500,
		"state": "running",
		"disk_bytes": 0
	}`, string(out))
}

// TestServer_ProcConcurrentWithStatsUpdates runs the stats listener's write
// path against websocket-style reads. go test -race must stay silent and
// nothing may block.
func TestServer_ProcConcurrentWithStatsUpdates(t *testing.T) {
	s := newResourceTestServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.resources.UpdateStats(environment.Stats{Memory: uint64(i*1000 + j)})
				if j%50 == 0 {
					s.resources.Reset()
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, err := json.Marshal(s.Proc())
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
}
