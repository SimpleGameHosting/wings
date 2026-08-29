package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
)

const testServerUUID = "8d2b3f6a-0000-4000-8000-000000000000"

// setNodeConfig installs the minimal global node configuration the server
// code reads during these tests; config.Set needs a token to derive the JWT
// signing key from, so it cannot be left empty.
func setNodeConfig(t *testing.T, crashDetectionEnabled bool) {
	t.Helper()
	config.Set(&config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     "/server",
			DiskCheckInterval: 150,
			CrashDetection:    config.CrashDetection{CrashDetectionEnabled: crashDetectionEnabled},
		},
	})
}

// syncSettings runs SyncWithConfiguration with a raw Panel settings payload.
func syncSettings(t *testing.T, s *Server, settings string, proc *remote.ProcessConfiguration) {
	t.Helper()
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings:             json.RawMessage(settings),
		ProcessConfiguration: proc,
	}))
}

// TestServer_SyncWithConfigurationReplacesSettings pins the sync contract: the
// Panel payload replaces the previous settings wholesale rather than merging
// into them, the node-level crash detection flag is the fallback when the Panel
// omits it, and the process configuration is swapped alongside the settings.
func TestServer_SyncWithConfigurationReplacesSettings(t *testing.T) {
	setNodeConfig(t, true)
	s := &Server{}
	proc := &remote.ProcessConfiguration{}

	syncSettings(t, s, `{
		"uuid": "`+testServerUUID+`",
		"suspended": true,
		"invocation": "java -jar server.jar",
		"build": {"disk_space": 2048, "memory_limit": 4096},
		"egg": {"id": "egg-1", "file_denylist": ["server.properties"]},
		"container": {"image": "ghcr.io/pterodactyl/yolks:java_17"}
	}`, proc)

	cfg := s.Config()
	assert.Equal(t, testServerUUID, cfg.Uuid)
	assert.True(t, cfg.Suspended)
	assert.Equal(t, "java -jar server.jar", cfg.Invocation)
	assert.True(t, cfg.CrashDetectionEnabled, "node default must apply when the Panel omits the flag")
	assert.Equal(t, []string{"server.properties"}, cfg.Egg.FileDenylist)
	assert.Equal(t, "ghcr.io/pterodactyl/yolks:java_17", cfg.Container.Image)
	assert.EqualValues(t, 2048*1024*1024, s.DiskSpace())
	assert.EqualValues(t, 4096, s.MemoryLimit())
	assert.Same(t, proc, s.ProcessConfiguration())

	// A second sync that omits earlier keys must drop them, not keep them.
	syncSettings(t, s, `{
		"uuid": "`+testServerUUID+`",
		"crash_detection_enabled": false,
		"build": {"disk_space": 1}
	}`, nil)

	cfg = s.Config()
	assert.False(t, cfg.Suspended)
	assert.Empty(t, cfg.Invocation)
	assert.False(t, cfg.CrashDetectionEnabled)
	assert.Empty(t, cfg.Egg.FileDenylist)
	assert.Empty(t, cfg.Container.Image)
	assert.EqualValues(t, 1024*1024, s.DiskSpace())
	assert.Zero(t, s.MemoryLimit())
	assert.Nil(t, s.ProcessConfiguration())
}

// TestServer_SyncWithConfigurationKeepsWaitersAlive hammers the configuration
// lock from reader and writer goroutines while the settings are re-synced
// underneath them. Replacing the whole Configuration struct, mutex included,
// overwrote the counters of every goroutine already queued on that mutex and
// left them blocked forever, so this test must finish promptly.
func TestServer_SyncWithConfigurationKeepsWaitersAlive(t *testing.T) {
	setNodeConfig(t, false)
	s := &Server{}
	settings := `{"uuid": "` + testServerUUID + `", "build": {"disk_space": 1}}`
	syncSettings(t, s, settings, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.DiskSpace()
					_ = s.Config().Uuid
					_ = s.Config().CrashDetectionEnabled
					s.SetSuspended(false)
				}
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		syncSettings(t, s, settings, nil)
	}
	close(stop)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutines waiting on the configuration lock never woke up after a sync")
	}
}

// TestServer_ConfigReturnsIsolatedSnapshot ensures callers cannot mutate live
// configuration values or their nested maps and slices outside the lock.
func TestServer_ConfigReturnsIsolatedSnapshot(t *testing.T) {
	setNodeConfig(t, false)
	s := &Server{}
	syncSettings(t, s, `{
		"uuid": "`+testServerUUID+`",
		"environment": {"SERVER_JARFILE": "server.jar"},
		"labels": {"tier": "gold"},
		"allocations": {"mappings": {"127.0.0.1": [25565]}},
		"mounts": [{"source": "/maps", "target": "/home/container/maps"}],
		"egg": {"file_denylist": ["server.properties"]}
	}`, nil)

	snapshot := s.Config()
	snapshot.Uuid = "mutated"
	snapshot.EnvVars["SERVER_JARFILE"] = "mutated.jar"
	snapshot.Labels["tier"] = "mutated"
	snapshot.Allocations.Mappings["127.0.0.1"][0] = 19132
	snapshot.Mounts[0].Source = "/mutated"
	snapshot.Egg.FileDenylist[0] = "mutated"

	live := s.Config()
	assert.Equal(t, testServerUUID, live.Uuid)
	assert.Equal(t, "server.jar", live.EnvVars.Get("SERVER_JARFILE"))
	assert.Equal(t, "gold", live.Labels["tier"])
	assert.Equal(t, []int{25565}, live.Allocations.Mappings["127.0.0.1"])
	assert.Equal(t, "/maps", live.Mounts[0].Source)
	assert.Equal(t, []string{"server.properties"}, live.Egg.FileDenylist)
}
