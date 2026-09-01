package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// TestSendPlayerEventsRetriesOnlyOnce ensures best-effort telemetry cannot
// occupy the shared cron for the generic client's full retry window.
func TestSendPlayerEventsRetriesOnlyOnce(t *testing.T) {
	attempts := 0
	c, server := createTestClient(func(rw http.ResponseWriter, _ *http.Request) {
		attempts++
		rw.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()
	c.maxAttempts = 2

	err := c.SendPlayerEvents(context.Background(), "some-uuid", []PlayerEventRequest{})

	assert.Error(t, err)
	assert.Equal(t, 2, attempts)
}

// TestSendPlayerEventsBoundsDeliveryTime ensures a hung telemetry callback
// cannot occupy the player-event cron for the generic 30-second retry window.
func TestSendPlayerEventsBoundsDeliveryTime(t *testing.T) {
	var remaining time.Duration
	c := &client{
		baseUrl: "http://panel.test",
		httpClient: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			deadline, ok := request.Context().Deadline()
			assert.True(t, ok)
			remaining = time.Until(deadline)

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
	}

	err := c.SendPlayerEvents(context.Background(), "some-uuid", []PlayerEventRequest{})

	assert.NoError(t, err)
	assert.Positive(t, remaining)
	assert.LessOrEqual(t, remaining, playerEventTimeout)
}

func TestSendModpackInstallResult(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/servers/abc-uuid/modpack-install-result", r.URL.Path)

		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		var data map[string]interface{}
		assert.NoError(t, json.Unmarshal(b, &data))
		assert.Equal(t, "11111111-2222-3333-4444-555555555555", data["install_id"])
		assert.Equal(t, true, data["successful"])
		assert.EqualValues(t, 4200, data["duration_ms"])

		rw.WriteHeader(http.StatusNoContent)
	})

	err := c.SendModpackInstallResult(context.Background(), "abc-uuid", ModpackInstallResultRequest{
		InstallID:  "11111111-2222-3333-4444-555555555555",
		Successful: true,
		DurationMs: 4200,
	})
	assert.NoError(t, err)
}

// TestSendModpackInstallResultRetriesOnFailure ensures a single transient
// panel error while reporting a finished install is absorbed by the one
// allowed retry instead of being dropped outright.
func TestSendModpackInstallResultRetriesOnFailure(t *testing.T) {
	attempts := 0
	c, server := createTestClient(func(rw http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()

	err := c.SendModpackInstallResult(context.Background(), "abc-uuid", ModpackInstallResultRequest{
		InstallID:  "11111111-2222-3333-4444-555555555555",
		Successful: false,
		Error:      "download failed",
	})

	assert.NoError(t, err)
	assert.Equal(t, 2, attempts)
}
