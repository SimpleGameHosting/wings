package remote

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type playerEventFixture struct {
	Patterns PlayerEventConfiguration `json:"patterns"`
	Samples  []struct {
		Line   string `json:"line"`
		Event  string `json:"event"`
		Code   string `json:"code"`
		Player string `json:"player"`
		Reason string `json:"reason"`
	} `json:"samples"`
}

func TestPlayerEventFixture_EverySampleMatchesItsPattern(t *testing.T) {
	raw, err := os.ReadFile("testdata/player_events_minecraft_java.json")
	require.NoError(t, err)

	var fx playerEventFixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	require.NotEmpty(t, fx.Samples)

	for _, sample := range fx.Samples {
		line := []byte(sample.Line)

		if sample.Event == "join" {
			matched := false
			for _, m := range fx.Patterns.Join {
				if m.Matches(line) {
					assert.Equal(t, sample.Player, m.Extract(line)["player"], "join line: %s", sample.Line)
					matched = true
					break
				}
			}
			assert.True(t, matched, "no join pattern matched: %s", sample.Line)
			continue
		}

		// failed_join: the FIRST matching pattern must be the expected code,
		// which proves the catch-all ordering is correct.
		var firstCode, firstReason, firstPlayer string
		for _, m := range fx.Patterns.FailedJoin {
			if m.Matcher.Matches(line) {
				groups := m.Matcher.Extract(line)
				firstCode, firstPlayer, firstReason = m.Code, groups["player"], groups["reason"]
				break
			}
		}
		assert.Equal(t, sample.Code, firstCode, "failed_join line: %s", sample.Line)
		assert.Equal(t, sample.Player, firstPlayer, "failed_join player: %s", sample.Line)
		if sample.Reason != "" {
			assert.Equal(t, sample.Reason, firstReason, "failed_join reason: %s", sample.Line)
		}
	}
}
