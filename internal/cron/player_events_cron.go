package cron

import (
	"context"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

// playerEventBatchMax is the largest number of events the Panel accepts in a
// single player-events request. A batch over this size is rejected outright
// (HTTP 422) and is not retried, so a drained slice larger than this must be
// split rather than sent whole.
const playerEventBatchMax = 20

// playerEventsCron drains each server's buffered player events and posts them
// to the Panel in bounded batches. A send failure for one server is logged and
// skipped so a single unreachable call does not prevent later servers sending.
type playerEventsCron struct {
	mu      *system.AtomicBool
	manager *server.Manager
}

// Run drains and sends every server's buffered player events. It refuses to
// run concurrently with itself, matching the other panel-batch crons.
func (pec *playerEventsCron) Run(ctx context.Context) error {
	if !pec.mu.SwapIf(true) {
		return errors.WithStack(ErrCronRunning)
	}
	defer pec.mu.Store(false)

	for _, s := range pec.manager.All() {
		events := s.DrainPlayerEvents()
		if len(events) == 0 {
			continue
		}

		if err := sendPlayerEventBatches(ctx, pec.manager.Client(), s.ID(), events); err != nil {
			log.WithField("subsystem", "cron").
				WithField("cron", "player-events").
				WithField("server", s.ID()).
				WithField("error", err).
				Warn("failed to send player events to Panel")
		}
	}

	return nil
}

// sendPlayerEventBatches delivers one server's drained events in chunks the
// Panel accepts, stopping after the first failed chunk.
func sendPlayerEventBatches(ctx context.Context, client remote.Client, uuid string, events []remote.PlayerEventRequest) error {
	// The drained slice can exceed the Panel's batch cap because the fixed
	// rate-limit window can admit events on both sides of its boundary. Send
	// chunks the Panel always accepts so one oversized request cannot lose the
	// entire drain...
	for start := 0; start < len(events); start += playerEventBatchMax {
		end := min(start+playerEventBatchMax, len(events))
		if err := client.SendPlayerEvents(ctx, uuid, events[start:end]); err != nil {
			return err
		}
	}

	return nil
}
