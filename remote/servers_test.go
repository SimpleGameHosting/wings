package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReportCrash(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/servers/some-uuid/crash", r.URL.Path)

		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		var data map[string]interface{}
		assert.NoError(t, json.Unmarshal(b, &data))
		assert.EqualValues(t, 137, data["exit_code"])
		assert.Equal(t, true, data["oom_killed"])
		assert.EqualValues(t, 42, data["uptime_seconds"])
		assert.NotEmpty(t, data["occurred_at"])
	})

	err := c.ReportCrash(context.Background(), "some-uuid", CrashReportRequest{
		ExitCode:      137,
		OOMKilled:     true,
		UptimeSeconds: 42,
		OccurredAt:    time.Now(),
	})
	assert.NoError(t, err)
}

func TestReportCrashPanelUnavailable(t *testing.T) {
	c, s := createTestClient(func(rw http.ResponseWriter, r *http.Request) {})
	s.Close()

	err := c.ReportCrash(context.Background(), "some-uuid", CrashReportRequest{})
	assert.Error(t, err)
}

func TestSendPlayerEvents(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/servers/some-uuid/player-events", r.URL.Path)

		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		var data map[string]interface{}
		assert.NoError(t, json.Unmarshal(b, &data))
		events := data["events"].([]interface{})
		assert.Len(t, events, 1)
		first := events[0].(map[string]interface{})
		assert.Equal(t, "failed_join", first["event"])
		assert.Equal(t, "not_whitelisted", first["code"])
		assert.Equal(t, "Sam", first["player"])
	})

	err := c.SendPlayerEvents(context.Background(), "some-uuid", []PlayerEventRequest{{
		Event:      "failed_join",
		Code:       "not_whitelisted",
		Player:     "Sam",
		Reason:     "You are not whitelisted on this server!",
		Line:       "Disconnecting Sam (/1.2.3.4:5): You are not whitelisted on this server!",
		OccurredAt: time.Now(),
	}})
	assert.NoError(t, err)
}

func TestSendPlayerEventsPanelUnavailable(t *testing.T) {
	c, s := createTestClient(func(rw http.ResponseWriter, r *http.Request) {})
	s.Close()

	err := c.SendPlayerEvents(context.Background(), "some-uuid", []PlayerEventRequest{})
	assert.Error(t, err)
}
