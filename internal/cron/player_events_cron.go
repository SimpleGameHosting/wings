package cron

import (
	"context"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

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

		if err := pec.manager.Client().SendPlayerEvents(ctx, s.ID(), events); err != nil {
			log.WithField("subsystem", "cron").
				WithField("cron", "player-events").
				WithField("server", s.ID()).
				WithField("error", err).
				Warn("failed to send player events to Panel")
		}
	}

	return nil
}
