package cron

import (
	"context"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

// playerEventBatchMax is the largest number of events the Panel accepts in a
// single player-events request. A batch over this size is rejected outright
// (HTTP 422) and is not retried, so a drained slice larger than this must be
// split rather than sent whole.
const playerEventBatchMax = 20

// playerEventsCron drains each server's buffered player events and posts them
// to the Panel, one batch per server. A send failure for one server is logged
// and skipped so a single unreachable call does not stall the others.
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

		// The drained slice can exceed the Panel's batch cap: the per-server
		// rate limiter is a fixed window and can admit close to double its
		// limit across a window boundary, and distinct players are never
		// collapsed by the drain's dedupe. Send in chunks the Panel will
		// always accept, rather than risking one oversized request losing
		// the whole batch...
		for start := 0; start < len(events); start += playerEventBatchMax {
			end := start + playerEventBatchMax
			if end > len(events) {
				end = len(events)
			}

			if err := pec.manager.Client().SendPlayerEvents(ctx, s.ID(), events[start:end]); err != nil {
				log.WithField("subsystem", "cron").
					WithField("cron", "player-events").
					WithField("server", s.ID()).
					WithField("error", err).
					Warn("failed to send player events to Panel")
				break
			}
		}
	}

	return nil
}
