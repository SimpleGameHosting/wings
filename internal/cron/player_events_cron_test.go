package cron

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

type fakePlayerEventClient struct {
	remote.Client
	mu          sync.Mutex
	sent        map[string][]remote.PlayerEventRequest
	callLengths map[string][]int
}

func (f *fakePlayerEventClient) SendPlayerEvents(_ context.Context, uuid string, events []remote.PlayerEventRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string][]remote.PlayerEventRequest{}
	}
	if f.callLengths == nil {
		f.callLengths = map[string][]int{}
	}
	f.sent[uuid] = append(f.sent[uuid], events...)
	f.callLengths[uuid] = append(f.callLengths[uuid], len(events))
	return nil
}

func TestPlayerEventsCron_DrainsAndSends(t *testing.T) {
	// SyncWithConfiguration reads the global node config, so the minimal
	// config must be installed before it is called from this test binary.
	config.Set(&config.Configuration{AuthenticationToken: "abc"})

	client := &fakePlayerEventClient{}
	// NewEmptyManager avoids NewManager's API boot-strap call, which would
	// invoke GetServers on the fake client's embedded nil remote.Client.
	m := server.NewEmptyManager(client)

	s, err := server.New(client)
	require.NoError(t, err)
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings:             []byte(`{}`),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}))
	// Seed a buffered event through the exported drain contract's counterpart.
	s.SeedPlayerEventForTest(remote.PlayerEventRequest{Event: "join", Player: "Bob"})
	m.Add(s)

	c := playerEventsCron{mu: system.NewAtomicBool(false), manager: m}
	require.NoError(t, c.Run(context.Background()))

	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.sent[s.ID()], 1)
	assert.Equal(t, "Bob", client.sent[s.ID()][0].Player)
	assert.Nil(t, s.DrainPlayerEvents())
}

func TestPlayerEventsCron_ChunksBatchesAtPanelMax(t *testing.T) {
	// SyncWithConfiguration reads the global node config, so the minimal
	// config must be installed before it is called from this test binary.
	config.Set(&config.Configuration{AuthenticationToken: "abc"})

	client := &fakePlayerEventClient{}
	m := server.NewEmptyManager(client)

	s, err := server.New(client)
	require.NoError(t, err)
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings:             []byte(`{}`),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}))

	// Seed more events than the Panel accepts in a single request. Each uses
	// a distinct player so DrainPlayerEvents' dedupe cannot collapse them,
	// mirroring how distinct real players would never be deduped either.
	const seeded = 25
	for i := 0; i < seeded; i++ {
		s.SeedPlayerEventForTest(remote.PlayerEventRequest{Event: "join", Player: fmt.Sprintf("Player%02d", i)})
	}
	m.Add(s)

	c := playerEventsCron{mu: system.NewAtomicBool(false), manager: m}
	require.NoError(t, c.Run(context.Background()))

	client.mu.Lock()
	defer client.mu.Unlock()

	lengths := client.callLengths[s.ID()]
	require.NotEmpty(t, lengths, "expected the cron to call SendPlayerEvents at least once")

	total := 0
	for _, n := range lengths {
		assert.LessOrEqualf(t, n, playerEventBatchMax, "a single SendPlayerEvents call must not exceed the Panel's batch cap")
		total += n
	}
	assert.Equal(t, seeded, total, "every seeded event must reach the Panel across the batched calls")
	assert.Greaterf(t, len(lengths), 1, "seeding %d events over a cap of %d must take more than one call", seeded, playerEventBatchMax)
}
