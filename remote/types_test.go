package remote

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputLineMatcher_ExtractNamedGroups(t *testing.T) {
	var m OutputLineMatcher
	require.NoError(t, json.Unmarshal([]byte(`"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): (?P<reason>.+)$"`), &m))

	caps := m.Extract([]byte("[17:42:48 INFO]: Disconnecting Sam (/172.17.0.1:58774): You are not whitelisted on this server!"))
	require.NotNil(t, caps)
	assert.Equal(t, "Sam", caps["player"])
	assert.Equal(t, "You are not whitelisted on this server!", caps["reason"])
}

func TestOutputLineMatcher_ExtractReturnsNilForSubstringMatcher(t *testing.T) {
	var m OutputLineMatcher
	require.NoError(t, json.Unmarshal([]byte(`"joined the game"`), &m))

	assert.Nil(t, m.Extract([]byte("Bob joined the game")))
}

func TestOutputLineMatcher_ExtractReturnsNilWhenNoMatch(t *testing.T) {
	var m OutputLineMatcher
	require.NoError(t, json.Unmarshal([]byte(`"regex:(?P<player>[A-Za-z0-9_]{3,16}) joined the game"`), &m))

	assert.Nil(t, m.Extract([]byte("nothing here")))
}

func TestFailedJoinMatcher_UnmarshalCodeAndMatch(t *testing.T) {
	var f FailedJoinMatcher
	require.NoError(t, json.Unmarshal([]byte(`{"code":"banned","match":"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): You are banned"}`), &f))

	assert.Equal(t, "banned", f.Code)
	require.NotNil(t, f.Matcher)
	assert.True(t, f.Matcher.Matches([]byte("Disconnecting Bob (/1.2.3.4:5): You are banned from this server.")))
	assert.Equal(t, "Bob", f.Matcher.Extract([]byte("Disconnecting Bob (/1.2.3.4:5): You are banned from this server."))["player"])
}

func TestProcessConfiguration_UnmarshalsPlayerEvents(t *testing.T) {
	var pc ProcessConfiguration
	require.NoError(t, json.Unmarshal([]byte(`{
		"startup": {"done": [")! For help, type "], "strip_ansi": false},
		"stop": {"type": "command", "value": "stop"},
		"configs": [],
		"player_events": {
			"join": ["regex:(?P<player>[A-Za-z0-9_]{3,16}) joined the game"],
			"failed_join": [{"code":"banned","match":"regex:Disconnecting (?P<player>[A-Za-z0-9_]{3,16}) \\(/[^)]+\\): You are banned"}]
		}
	}`), &pc))

	require.Len(t, pc.PlayerEvents.Join, 1)
	require.Len(t, pc.PlayerEvents.FailedJoin, 1)
	assert.Equal(t, "banned", pc.PlayerEvents.FailedJoin[0].Code)
	assert.True(t, pc.PlayerEvents.Join[0].Matches([]byte("Alex joined the game")))
}

func TestProcessConfiguration_MissingPlayerEventsIsEmpty(t *testing.T) {
	var pc ProcessConfiguration
	require.NoError(t, json.Unmarshal([]byte(`{"startup":{"done":[")! For help, type "]},"stop":{"type":"command","value":"stop"},"configs":[]}`), &pc))

	assert.Empty(t, pc.PlayerEvents.Join)
	assert.Empty(t, pc.PlayerEvents.FailedJoin)
}
