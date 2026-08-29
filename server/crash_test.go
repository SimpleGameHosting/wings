package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
)

// crashReportTestClient captures callback payloads while leaving unrelated
// remote-client methods unused through the embedded interface.
type crashReportTestClient struct {
	remote.Client
	reports chan remote.CrashReportRequest
}

// ReportCrash captures a crash callback for deterministic assertions.
func (c crashReportTestClient) ReportCrash(_ context.Context, _ string, report remote.CrashReportRequest) error {
	c.reports <- report

	return nil
}

// crashReportTestEnvironment provides the offline exit state consumed by the
// crash handler while leaving unrelated environment operations unused.
type crashReportTestEnvironment struct {
	environment.ProcessEnvironment
	exitCode  uint32
	oomKilled bool
}

// State reports that the process is offline and eligible for crash handling.
func (e crashReportTestEnvironment) State() string {
	return environment.ProcessOfflineState
}

// ExitState returns the process result included in the Panel callback.
func (e crashReportTestEnvironment) ExitState() (uint32, bool, error) {
	return e.exitCode, e.oomKilled, nil
}

// TestReportCrashToPanelCapturesUptimeBeforeDispatch ensures later server
// state changes cannot rewrite an already-detected crash's payload.
func TestReportCrashToPanelCapturesUptimeBeforeDispatch(t *testing.T) {
	client := crashReportTestClient{reports: make(chan remote.CrashReportRequest, 1)}
	s, err := New(client)
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)
	syncSettings(t, s, `{"uuid":"`+testServerUUID+`"}`, nil)
	s.reportCrashToPanel(137, true, 90500)

	select {
	case report := <-client.reports:
		assert.EqualValues(t, 90, report.UptimeSeconds)
		assert.EqualValues(t, 137, report.ExitCode)
		assert.True(t, report.OOMKilled)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for crash callback")
	}
}

// TestHandleServerCrashReportsBeforeRestartThrottle ensures every detected
// crash reaches the callback even when Wings refuses an automatic restart.
func TestHandleServerCrashReportsBeforeRestartThrottle(t *testing.T) {
	setNodeConfig(t, true)
	config.Update(func(c *config.Configuration) {
		c.System.CrashDetection.Timeout = 60
	})

	client := crashReportTestClient{reports: make(chan remote.CrashReportRequest, 1)}
	s, err := New(client)
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)
	syncSettings(t, s, `{"uuid":"`+testServerUUID+`","crash_detection_enabled":true}`, nil)
	s.Environment = crashReportTestEnvironment{exitCode: 137, oomKilled: true}
	s.crasher.SetLastCrash(time.Now())

	err = s.handleServerCrash(90500)
	require.Error(t, err)
	assert.True(t, IsTooFrequentCrashError(err))

	select {
	case report := <-client.reports:
		assert.EqualValues(t, 137, report.ExitCode)
		assert.True(t, report.OOMKilled)
		assert.EqualValues(t, 90, report.UptimeSeconds)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for throttled crash callback")
	}
}
