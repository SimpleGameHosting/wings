package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/setupapply"
	"github.com/pterodactyl/wings/remote"
)

// setupApplyTestClient captures the terminal report a job sends the panel,
// and answers the configuration sync the start step performs so the job can
// reach the environment without a real panel behind it.
type setupApplyTestClient struct {
	remote.Client

	results  chan remote.SetupApplyResultRequest
	settings json.RawMessage
}

func (c setupApplyTestClient) SendSetupApplyResult(_ context.Context, _ string, data remote.SetupApplyResultRequest) error {
	c.results <- data
	return nil
}

func (c setupApplyTestClient) GetServerConfiguration(_ context.Context, _ string) (remote.ServerConfigurationResponse, error) {
	return setupApplyServerConfiguration(c.settings), nil
}

// setupApplyServerConfiguration is the shape the panel really sends: a
// settings blob plus a process configuration, which the start step's
// pre-boot pass over the configuration files reads and would otherwise
// dereference as nil.
func setupApplyServerConfiguration(settings json.RawMessage) remote.ServerConfigurationResponse {
	return remote.ServerConfigurationResponse{
		Settings:             settings,
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}
}

// setupApplyTestEnvironment is an offline environment whose stop always
// succeeds instantly and whose start records that it was asked, so the
// pipeline runs its apply step against a real temp filesystem without
// Docker.
type setupApplyTestEnvironment struct {
	environment.ProcessEnvironment

	started chan struct{}
	state   string
	config  *environment.Configuration
}

func (e *setupApplyTestEnvironment) State() string { return e.state }

// WaitForStop parks the fixture in the offline state, the same observable
// outcome a real environment reaches once the process is gone.
func (e *setupApplyTestEnvironment) WaitForStop(context.Context, time.Duration, bool) error {
	e.state = environment.ProcessOfflineState
	return nil
}

func (e *setupApplyTestEnvironment) Start(context.Context) error {
	e.started <- struct{}{}
	return nil
}

func (e *setupApplyTestEnvironment) OnBeforeStart(context.Context) error { return nil }
func (e *setupApplyTestEnvironment) Events() *events.Bus                 { return events.NewBus() }
func (e *setupApplyTestEnvironment) Config() *environment.Configuration  { return e.config }
func (e *setupApplyTestEnvironment) InSituUpdate() error                 { return nil }

func newSetupApplyServer(t *testing.T) (*Server, setupApplyTestClient, *setupApplyTestEnvironment) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	next.System.SetupApply.TimeoutMinutes = 1
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	settings := json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, uuid.NewString()))
	client := setupApplyTestClient{results: make(chan remote.SetupApplyResultRequest, 2), settings: settings}
	m := NewEmptyManager(client)
	s, err := m.InitServer(setupApplyServerConfiguration(settings))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)

	env := &setupApplyTestEnvironment{
		started: make(chan struct{}, 1),
		state:   environment.ProcessOfflineState,
		config:  environment.NewConfiguration(environment.Settings{}, nil),
	}
	s.Environment = env
	return s, client, env
}

// TestRunSetupApplyWritesFilesStartsAndReportsSuccess runs a full request
// through the job and checks the files, the start, the callback, and that
// the reservation is free again afterwards.
func TestRunSetupApplyWritesFilesStartsAndReportsSuccess(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	req := setupapply.Request{
		SetupID:    uuid.NewString(),
		Eula:       true,
		Operators:  []setupapply.Operator{{UUID: uuid.NewString(), Name: "Kane", Level: 4}},
		Properties: map[string]string{"white-list": "true"},
	}
	if repeat, err := s.AdmitSetupApply(req.SetupID); err != nil || repeat {
		t.Fatalf("admit: repeat=%v err=%v", repeat, err)
	}

	s.RunSetupApply(req)

	select {
	case <-env.started:
	default:
		t.Fatal("the job never asked the environment to start")
	}
	result := <-client.results
	if !result.Successful || result.SetupID != req.SetupID || result.ErrorCode != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, name := range []string{setupapply.EulaFileName, setupapply.OpsFileName, setupapply.PropertiesFileName} {
		if _, err := s.Filesystem().UnixFS().Lstat(name); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	if s.CurrentOperation() != "" || s.ActiveSetupApplyID() != "" {
		t.Fatal("reservation or id still held after the job finished")
	}
	if repeat, _ := s.AdmitSetupApply(req.SetupID); !repeat {
		t.Fatal("finished id must be remembered as a repeat")
	}
}

// TestRunSetupApplyReportsApplyFailedOnMalformedOps proves a corrupt game
// file fails the attempt with the stage code, never starts the server, and
// still releases everything.
func TestRunSetupApplyReportsApplyFailedOnMalformedOps(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	if err := s.Filesystem().Write(setupapply.OpsFileName, strings.NewReader("{broken"), 7, 0o644); err != nil {
		t.Fatal(err)
	}
	req := setupapply.Request{SetupID: uuid.NewString(), Operators: []setupapply.Operator{{UUID: uuid.NewString(), Name: "Kane", Level: 4}}}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if result.Successful || result.ErrorCode != SetupApplyErrorApplyFailed {
		t.Fatalf("unexpected result: %+v", result)
	}
	select {
	case <-env.started:
		t.Fatal("a failed apply must not start the server")
	default:
	}
	if s.CurrentOperation() != "" {
		t.Fatal("reservation still held after a failed job")
	}
}

// TestRunSetupApplyEmptyRequestOnlyRestarts proves the Bedrock shape stops
// and starts without touching any file.
func TestRunSetupApplyEmptyRequestOnlyRestarts(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	env.state = environment.ProcessRunningState
	req := setupapply.Request{SetupID: uuid.NewString()}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	if result := <-client.results; !result.Successful {
		t.Fatalf("unexpected result: %+v", result)
	}
	<-env.started
	for _, name := range []string{setupapply.EulaFileName, setupapply.OpsFileName, setupapply.WhitelistFileName, setupapply.PropertiesFileName} {
		if _, err := s.Filesystem().UnixFS().Lstat(name); err == nil {
			t.Fatalf("%s must not be written by an empty request", name)
		}
	}
}
