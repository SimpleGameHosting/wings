package cron

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/remote"
)

// fakePlayerEventClient records delivered batches for cron assertions.
type fakePlayerEventClient struct {
	remote.Client
	mu          sync.Mutex
	sent        map[string][]remote.PlayerEventRequest
	callLengths map[string][]int
	calls       int
	failAtCall  int
}

// SendPlayerEvents records one callback batch.
func (f *fakePlayerEventClient) SendPlayerEvents(_ context.Context, uuid string, events []remote.PlayerEventRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string][]remote.PlayerEventRequest{}
	}
	if f.callLengths == nil {
		f.callLengths = map[string][]int{}
	}
	f.calls++
	if f.calls == f.failAtCall {
		return errors.New("panel unavailable")
	}
	f.sent[uuid] = append(f.sent[uuid], events...)
	f.callLengths[uuid] = append(f.callLengths[uuid], len(events))
	return nil
}

// TestSendPlayerEventBatchesSendsEvents verifies an ordinary batch is delivered.
func TestSendPlayerEventBatchesSendsEvents(t *testing.T) {
	client := &fakePlayerEventClient{}
	uuid := "server"
	require.NoError(t, sendPlayerEventBatches(context.Background(), client, uuid, []remote.PlayerEventRequest{{
		Event:  "join",
		Player: "Bob",
	}}))

	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.sent[uuid], 1)
	assert.Equal(t, "Bob", client.sent[uuid][0].Player)
}

// TestSendPlayerEventBatchesChunksAtPanelMax verifies oversized drains are
// divided without dropping any events.
func TestSendPlayerEventBatchesChunksAtPanelMax(t *testing.T) {
	client := &fakePlayerEventClient{}
	uuid := "server"

	// Stage more events than the Panel accepts in one request to verify every
	// chunk stays within the callback contract...
	const seeded = 25
	events := make([]remote.PlayerEventRequest, 0, seeded)
	for i := 0; i < seeded; i++ {
		events = append(events, remote.PlayerEventRequest{Event: "join", Player: fmt.Sprintf("Player%02d", i)})
	}
	require.NoError(t, sendPlayerEventBatches(context.Background(), client, uuid, events))

	client.mu.Lock()
	defer client.mu.Unlock()

	lengths := client.callLengths[uuid]
	require.NotEmpty(t, lengths, "expected the cron to call SendPlayerEvents at least once")

	total := 0
	for _, n := range lengths {
		assert.LessOrEqualf(t, n, playerEventBatchMax, "a single SendPlayerEvents call must not exceed the Panel's batch cap")
		total += n
	}
	assert.Equal(t, seeded, total, "every seeded event must reach the Panel across the batched calls")
	assert.Greaterf(t, len(lengths), 1, "seeding %d events over a cap of %d must take more than one call", seeded, playerEventBatchMax)
}

// TestSendPlayerEventBatchesStopsAfterFailure ensures later chunks are not sent
// out of order after a callback failure.
func TestSendPlayerEventBatchesStopsAfterFailure(t *testing.T) {
	client := &fakePlayerEventClient{failAtCall: 2}
	events := make([]remote.PlayerEventRequest, playerEventBatchMax*3)

	err := sendPlayerEventBatches(context.Background(), client, "server", events)

	require.Error(t, err)
	assert.Equal(t, 2, client.calls)
	assert.Len(t, client.sent["server"], playerEventBatchMax)
}
