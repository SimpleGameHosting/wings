package server

import (
	"sync"
	"time"

	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// playerEventMaxPerMinute caps how many player events a single server may
// buffer per minute; excess is dropped so a noisy or hostile server cannot
// flood the panel callback.
const playerEventMaxPerMinute = 20

// playerEventLineMax is the longest console line stored on an event, matching
// the panel's player_join_failures.line column width.
const playerEventLineMax = 512

// playerEventReasonMax is the longest failed-join reason stored on an event,
// matching the panel's player_join_failures.reason column width. The panel
// rejects the whole batch outright when a reason is over length, so this
// must be enforced before the event is buffered.
const playerEventReasonMax = 255

// playerEventCodeMax is the longest failed-join code stored on an event,
// matching the panel's player_join_failures.code column width. Codes are
// sourced from the panel's own egg configuration, so this bound is
// defensive rather than one expected to trigger in practice.
const playerEventCodeMax = 32

// playerEventBuffer holds the player events detected for one server between
// cron drains, behind its own lock and rate limiter.
type playerEventBuffer struct {
	mu     sync.Mutex
	events []remote.PlayerEventRequest
	rate   *system.Rate
}

// matchAndBufferPlayerEvents tests one console line against the panel-served
// join and failed-join matchers and buffers the first one that matches. Joins
// are checked before failed joins, and each list is evaluated in the panel's
// order so its catch-all sits last. A matched join line whose player name is
// empty is dropped rather than misfiled as a failed join.
func (s *Server) matchAndBufferPlayerEvents(cfg *remote.ProcessConfiguration, line []byte) {
	if cfg == nil {
		return
	}

	events := cfg.PlayerEvents
	if len(events.Join) == 0 && len(events.FailedJoin) == 0 {
		return
	}

	// Strip ANSI when the egg asks for it, so colour codes never break a match.
	v := line
	if cfg.Startup.StripAnsi {
		v = stripAnsiRegex.ReplaceAll(v, []byte(""))
	}

	for _, m := range events.Join {
		if !m.Matches(v) {
			continue
		}

		player := m.Extract(v)["player"]
		if player == "" {
			return
		}

		s.recordPlayerEvent(remote.PlayerEventRequest{
			Event:      "join",
			Player:     player,
			Line:       truncatePlayerEventLine(v),
			OccurredAt: time.Now(),
		})

		return
	}

	for _, m := range events.FailedJoin {
		if m.Matcher == nil || !m.Matcher.Matches(v) {
			continue
		}

		groups := m.Matcher.Extract(v)
		s.recordPlayerEvent(remote.PlayerEventRequest{
			Event:      "failed_join",
			Code:       truncatePlayerEventCode(m.Code),
			Player:     groups["player"],
			Reason:     truncatePlayerEventReason(groups["reason"]),
			Line:       truncatePlayerEventLine(v),
			OccurredAt: time.Now(),
		})

		return
	}
}

// recordPlayerEvent appends an event to the buffer unless the per-minute rate
// limit has been hit, in which case it is dropped with a debug log.
func (s *Server) recordPlayerEvent(event remote.PlayerEventRequest) {
	s.playerEvents.mu.Lock()
	defer s.playerEvents.mu.Unlock()

	if s.playerEvents.rate == nil {
		s.playerEvents.rate = system.NewRate(playerEventMaxPerMinute, time.Minute)
	}

	if !s.playerEvents.rate.Try() {
		s.Log().Debug("player events: per-minute limit reached, dropping event")

		return
	}

	s.playerEvents.events = append(s.playerEvents.events, event)
}

// DrainPlayerEvents removes and returns the buffered events for this server,
// collapsing identical (event, code, player) tuples so a doubly-logged
// rejection is reported once. It returns nil when nothing is buffered.
func (s *Server) DrainPlayerEvents() []remote.PlayerEventRequest {
	s.playerEvents.mu.Lock()
	defer s.playerEvents.mu.Unlock()

	if len(s.playerEvents.events) == 0 {
		return nil
	}

	buffered := s.playerEvents.events
	s.playerEvents.events = nil

	seen := make(map[string]struct{}, len(buffered))
	deduped := make([]remote.PlayerEventRequest, 0, len(buffered))
	for _, event := range buffered {
		key := event.Event + "\x00" + event.Code + "\x00" + event.Player
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, event)
	}

	return deduped
}

// SeedPlayerEventForTest appends an event directly to the buffer, bypassing
// matching and the rate limit. It exists so cron tests can stage buffered
// events without synthesising console output.
func (s *Server) SeedPlayerEventForTest(event remote.PlayerEventRequest) {
	s.playerEvents.mu.Lock()
	defer s.playerEvents.mu.Unlock()
	s.playerEvents.events = append(s.playerEvents.events, event)
}

// truncatePlayerEventLine trims a console line to the stored maximum length.
func truncatePlayerEventLine(line []byte) string {
	if len(line) > playerEventLineMax {
		return string(line[:playerEventLineMax])
	}

	return string(line)
}

// truncatePlayerEventReason trims a failed-join reason to the stored maximum
// length. The catch-all failed-join pattern captures the rest of the console
// line as the reason, so a verbose plugin or mod kick message must be
// clamped here before the panel ever sees it.
func truncatePlayerEventReason(reason string) string {
	if len(reason) > playerEventReasonMax {
		return reason[:playerEventReasonMax]
	}

	return reason
}

// truncatePlayerEventCode trims a failed-join code to the stored maximum
// length. Codes come from the panel's own egg configuration, so this is a
// defensive bound rather than one expected to trigger in practice.
func truncatePlayerEventCode(code string) string {
	if len(code) > playerEventCodeMax {
		return code[:playerEventCodeMax]
	}

	return code
}
