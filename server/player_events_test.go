package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/remote"
)

func procWithPlayerEvents(t *testing.T) *remote.ProcessConfiguration {
	t.Helper()
	var pc remote.ProcessConfiguration
	require.NoError(t, json.Unmarshal([]byte(`{
		"startup": {"done": [")! For help, type "], "strip_ansi": false},
		"stop": {"type": "command", "value": "stop"},
		"configs": [],
		"player_events": {
			"join": [
				"regex:(?P<player>[A-Za-z0-9_]{3,16})\\[/[^\\]]+\\] logged in with entity id",
				"regex:(?P<player>[A-Za-z0-9_]{3,16}) joined the game"
			],
			"failed_join": [
				{"code":"not_whitelisted","match":"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): You are not white-?listed on this server!"},
				{"code":"banned","match":"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): You are banned from this server"},
				{"code":"rejected","match":"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): (?P<reason>.+)$"}
			]
		}
	}`), &pc))
	return &pc
}

func TestServer_MatchAndBufferPlayerEvents_Join(t *testing.T) {
	s := &Server{}
	s.matchAndBufferPlayerEvents(procWithPlayerEvents(t), []byte("[17:44:02 INFO]: Bob joined the game"))

	events := s.DrainPlayerEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "join", events[0].Event)
	assert.Equal(t, "Bob", events[0].Player)
	assert.Empty(t, events[0].Code)
}

func TestServer_MatchAndBufferPlayerEvents_NotWhitelisted(t *testing.T) {
	s := &Server{}
	s.matchAndBufferPlayerEvents(procWithPlayerEvents(t), []byte("[17:42:48 INFO]: Disconnecting Sam (/172.17.0.1:58774): You are not whitelisted on this server!"))

	events := s.DrainPlayerEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "failed_join", events[0].Event)
	assert.Equal(t, "not_whitelisted", events[0].Code)
	assert.Equal(t, "Sam", events[0].Player)
}

func TestServer_MatchAndBufferPlayerEvents_FirstMatchWinsCatchAllLast(t *testing.T) {
	s := &Server{}
	s.matchAndBufferPlayerEvents(procWithPlayerEvents(t), []byte("Disconnecting Alex (/1.2.3.4:5): Some brand new reason"))

	events := s.DrainPlayerEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "rejected", events[0].Code)
	assert.Equal(t, "Alex", events[0].Player)
	assert.Equal(t, "Some brand new reason", events[0].Reason)
}

func TestServer_MatchAndBufferPlayerEvents_IgnoresUnmatchedAndEmptyConfig(t *testing.T) {
	s := &Server{}
	s.matchAndBufferPlayerEvents(procWithPlayerEvents(t), []byte("[17:44:02 INFO]: Preparing spawn area: 0%"))
	assert.Nil(t, s.DrainPlayerEvents())

	s.matchAndBufferPlayerEvents(&remote.ProcessConfiguration{}, []byte("Bob joined the game"))
	assert.Nil(t, s.DrainPlayerEvents())

	s.matchAndBufferPlayerEvents(nil, []byte("Bob joined the game"))
	assert.Nil(t, s.DrainPlayerEvents())
}

func TestServer_MatchAndBufferPlayerEvents_TruncatesLongReason(t *testing.T) {
	s := &Server{}
	// A hostile or verbose plugin/mod kick message can push the catch-all
	// "reason" capture group well past the panel's 255-char column width;
	// the whole batch is 422-rejected (and lost) if it is not clamped here.
	longReason := strings.Repeat("x", 300)
	line := fmt.Sprintf("Disconnecting Alex (/1.2.3.4:5): %s", longReason)
	s.matchAndBufferPlayerEvents(procWithPlayerEvents(t), []byte(line))

	events := s.DrainPlayerEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "rejected", events[0].Code)
	assert.Equal(t, "Alex", events[0].Player)
	assert.Len(t, events[0].Reason, 255)
	assert.Equal(t, longReason[:255], events[0].Reason)
}

func TestTruncatePlayerEventReason(t *testing.T) {
	assert.Equal(t, "short", truncatePlayerEventReason("short"))

	long := strings.Repeat("y", playerEventReasonMax+10)
	truncated := truncatePlayerEventReason(long)
	assert.Len(t, truncated, playerEventReasonMax)
	assert.Equal(t, long[:playerEventReasonMax], truncated)
}

func TestTruncatePlayerEventCode(t *testing.T) {
	// Codes are panel-configured rather than free text, so this guards a
	// defensive bound rather than a scenario expected in practice.
	assert.Equal(t, "short", truncatePlayerEventCode("short"))

	long := strings.Repeat("z", playerEventCodeMax+10)
	truncated := truncatePlayerEventCode(long)
	assert.Len(t, truncated, playerEventCodeMax)
	assert.Equal(t, long[:playerEventCodeMax], truncated)
}

func TestServer_DrainPlayerEvents_DedupesIdenticalWithinBatch(t *testing.T) {
	s := &Server{}
	proc := procWithPlayerEvents(t)
	// Paper prints the disconnect line twice; both must collapse to one event.
	s.matchAndBufferPlayerEvents(proc, []byte("Disconnecting Sam (/1.2.3.4:5): You are not whitelisted on this server!"))
	s.matchAndBufferPlayerEvents(proc, []byte("Disconnecting Sam (/1.2.3.4:5): You are not whitelisted on this server!"))

	events := s.DrainPlayerEvents()
	assert.Len(t, events, 1)
}

func TestServer_DrainPlayerEvents_EmptyReturnsNil(t *testing.T) {
	s := &Server{}
	assert.Nil(t, s.DrainPlayerEvents())
}

func TestServer_RecordPlayerEvent_RateLimited(t *testing.T) {
	s := &Server{}
	proc := procWithPlayerEvents(t)
	// Each iteration uses a distinct player so every buffered event gets its
	// own dedupe key; only the rate limiter, not dedupe, can shrink the count.
	for i := 0; i < playerEventMaxPerMinute+5; i++ {
		line := fmt.Sprintf("Disconnecting Player%02d (/1.2.3.4:5): You are not whitelisted on this server!", i)
		s.matchAndBufferPlayerEvents(proc, []byte(line))
	}

	events := s.DrainPlayerEvents()
	assert.Len(t, events, playerEventMaxPerMinute)
}

func TestServer_OnConsoleOutput_BuffersPlayerEventsWhileRunning(t *testing.T) {
	setNodeConfig(t, false)
	s := &Server{}
	s.Environment = stateOnlyEnvironment{state: environmentRunningStateForTest()}
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings:             json.RawMessage(`{}`),
		ProcessConfiguration: procWithPlayerEvents(t),
	}))

	s.onConsoleOutput([]byte("[17:44:02 INFO]: Bob joined the game"))

	events := s.DrainPlayerEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "Bob", events[0].Player)
}

func environmentRunningStateForTest() string {
	return "running"
}
