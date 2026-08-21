package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/environment"
)

// stateOnlyEnvironment satisfies the environment interface for the one call
// ToAPIResponse makes on it; every other method would hit the nil embed.
type stateOnlyEnvironment struct {
	environment.ProcessEnvironment
	state string
}

func (e stateOnlyEnvironment) State() string {
	return e.state
}

// TestServer_ToAPIResponseJSON pins the full server payload the Panel reads
// from GET /api/servers and GET /api/servers/:server, covering both the
// utilization and the configuration sub-documents.
func TestServer_ToAPIResponseJSON(t *testing.T) {
	s := newResourceTestServer(t)
	s.Environment = stateOnlyEnvironment{state: environment.ProcessStartingState}
	s.resources.UpdateStats(environment.Stats{Memory: 2048, Uptime: 15000})
	syncSettings(t, s, `{
		"uuid": "`+testServerUUID+`",
		"meta": {"name": "Survival", "description": "Main world"},
		"suspended": true,
		"invocation": "java -jar server.jar",
		"skip_egg_scripts": true,
		"environment": {"SERVER_JARFILE": "server.jar"},
		"labels": {"tier": "gold"},
		"allocations": {"default": {"ip": "203.0.113.10", "port": 25565}, "mappings": {"203.0.113.10": [25565]}},
		"build": {"memory_limit": 4096, "swap": 0, "io_weight": 500, "cpu_limit": 200, "disk_space": 2048},
		"crash_detection_enabled": true,
		"mounts": [{"source": "/mnt/maps", "target": "/home/container/maps", "read_only": true}],
		"egg": {"id": "egg-1", "file_denylist": ["server.properties"]},
		"container": {"image": "ghcr.io/pterodactyl/yolks:java_17"}
	}`, nil)

	out, err := json.Marshal(s.ToAPIResponse())
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"state": "starting",
		"is_suspended": true,
		"utilization": {
			"memory_bytes": 2048,
			"memory_limit_bytes": 0,
			"cpu_absolute": 0,
			"network": {"rx_bytes": 0, "tx_bytes": 0},
			"uptime": 15000,
			"state": "offline",
			"disk_bytes": 0
		},
		"configuration": {
			"uuid": "`+testServerUUID+`",
			"meta": {"name": "Survival", "description": "Main world"},
			"suspended": true,
			"invocation": "java -jar server.jar",
			"skip_egg_scripts": true,
			"environment": {"SERVER_JARFILE": "server.jar"},
			"labels": {"tier": "gold"},
			"allocations": {
				"force_outgoing_ip": false,
				"default": {"ip": "203.0.113.10", "port": 25565},
				"mappings": {"203.0.113.10": [25565]}
			},
			"build": {
				"memory_limit": 4096,
				"swap": 0,
				"io_weight": 500,
				"cpu_limit": 200,
				"threads": "",
				"disk_space": 2048,
				"oom_disabled": false
			},
			"crash_detection_enabled": true,
			"mounts": [{"source": "/mnt/maps", "target": "/home/container/maps", "read_only": true}],
			"egg": {"id": "egg-1", "file_denylist": ["server.properties"]},
			"container": {"image": "ghcr.io/pterodactyl/yolks:java_17"}
		}
	}`, string(out))
}
