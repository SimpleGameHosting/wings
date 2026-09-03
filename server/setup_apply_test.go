package server

import (
	"context"
	"encoding/json"
	"errors"
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

	// stop replaces the default stop behavior when set, so a case can make
	// the stop step fail, stall until its deadline, or leave the server
	// running the way a racing user start would.
	stop func(ctx context.Context) error

	// startErr is what Start reports back after recording that it was
	// asked, so a case can fail the start step itself.
	startErr error
}

func (e *setupApplyTestEnvironment) State() string { return e.state }

// WaitForStop parks the fixture in the offline state, the same observable
// outcome a real environment reaches once the process is gone.
func (e *setupApplyTestEnvironment) WaitForStop(ctx context.Context, _ time.Duration, _ bool) error {
	if e.stop != nil {
		return e.stop(ctx)
	}
	e.state = environment.ProcessOfflineState
	return nil
}

func (e *setupApplyTestEnvironment) Start(context.Context) error {
	e.started <- struct{}{}
	return e.startErr
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

// setupApplyStatusRecorder subscribes to the server's event bus and returns
// a collector for every "setup apply status" event the job published, so a
// case can assert on the ordered contract the panel consumes rather than
// only on the final callback. Console output shares the same bus, so every
// other topic is skipped.
func setupApplyStatusRecorder(t *testing.T, s *Server) func() []SetupApplyStatus {
	t.Helper()

	sink := make(chan []byte, 64)
	s.Events().On(sink)
	t.Cleanup(func() { s.Events().Off(sink) })

	return func() []SetupApplyStatus {
		var collected []SetupApplyStatus
		for {
			select {
			case raw := <-sink:
				var event events.Event
				if err := events.DecodeTo(raw, &event); err != nil {
					t.Fatalf("undecodable event: %v", err)
				}
				if event.Topic != SetupApplyStatusEvent {
					continue
				}

				// The bus carries the payload as decoded JSON, so it goes
				// back through the encoder to reach the typed shape the
				// panel actually parses...
				encoded, err := json.Marshal(event.Data)
				if err != nil {
					t.Fatalf("unencodable status payload: %v", err)
				}
				var status SetupApplyStatus
				if err := json.Unmarshal(encoded, &status); err != nil {
					t.Fatalf("undecodable status payload: %v", err)
				}
				collected = append(collected, status)
			default:
				return collected
			}
		}
	}
}

// setupApplyStates flattens recorded events to their states so an ordering
// assertion reads as the sequence the panel sees.
func setupApplyStates(recorded []SetupApplyStatus) []string {
	states := make([]string, 0, len(recorded))
	for _, status := range recorded {
		states = append(states, status.State)
	}
	return states
}

// TestRunSetupApplyTreatsAnAlreadyRunningServerAsSuccess covers the race the
// job deliberately allows: it releases the reservation before starting, so a
// user's own start can win it and leave the server already running by the
// time the start step looks. HandlePowerAction reports ErrIsRunning for that,
// but the end state the job asked for was reached, so the attempt succeeds.
func TestRunSetupApplyTreatsAnAlreadyRunningServerAsSuccess(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	env.state = environment.ProcessRunningState
	env.stop = func(context.Context) error { return nil }

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if !result.Successful || result.ErrorCode != "" {
		t.Fatalf("an already running server must be a success: %+v", result)
	}
	select {
	case <-env.started:
		t.Fatal("the job must not start a server that is already running")
	default:
	}
	if s.CurrentOperation() != "" {
		t.Fatal("reservation still held after the job finished")
	}
}

// TestRunSetupApplyPublishesTheOrderedStatusEvents pins the websocket
// contract for a successful attempt: one event per pipeline step, in order,
// followed by the terminal completion.
func TestRunSetupApplyPublishesTheOrderedStatusEvents(t *testing.T) {
	s, client, _ := newSetupApplyServer(t)
	recorded := setupApplyStatusRecorder(t, s)

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	if result := <-client.results; !result.Successful {
		t.Fatalf("unexpected result: %+v", result)
	}
	states := setupApplyStates(recorded())
	want := []string{"stopping", "applying", "starting", "completed"}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i, state := range want {
		if states[i] != state {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

// TestRunSetupApplyPublishesAFailedTerminalEvent proves the terminal event
// of a failed attempt carries the same stable code and message the panel
// callback does, so a websocket consumer never has to wait for the panel to
// learn why the attempt failed.
func TestRunSetupApplyPublishesAFailedTerminalEvent(t *testing.T) {
	s, client, _ := newSetupApplyServer(t)
	if err := s.Filesystem().Write(setupapply.OpsFileName, strings.NewReader("{broken"), 7, 0o644); err != nil {
		t.Fatal(err)
	}
	recorded := setupApplyStatusRecorder(t, s)

	req := setupapply.Request{SetupID: uuid.NewString(), Operators: []setupapply.Operator{{UUID: uuid.NewString(), Name: "Kane", Level: 4}}}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	<-client.results
	published := recorded()
	if len(published) == 0 {
		t.Fatal("no status events were published")
	}
	terminal := published[len(published)-1]
	if terminal.State != "failed" || terminal.ErrorCode != SetupApplyErrorApplyFailed || terminal.Error == "" {
		t.Fatalf("unexpected terminal event: %+v", terminal)
	}
	if terminal.SetupID != req.SetupID {
		t.Fatalf("terminal event setup_id = %q, want %q", terminal.SetupID, req.SetupID)
	}
}

// TestRunSetupApplyReportsStopFailed proves a server that refuses to stop
// ends the attempt before anything is written or started.
func TestRunSetupApplyReportsStopFailed(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	env.state = environment.ProcessRunningState
	env.stop = func(context.Context) error { return errors.New("setup apply test fixture: nothing to stop") }

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if result.Successful || result.ErrorCode != SetupApplyErrorStopFailed {
		t.Fatalf("unexpected result: %+v", result)
	}
	select {
	case <-env.started:
		t.Fatal("a failed stop must not start the server")
	default:
	}
	if _, err := s.Filesystem().UnixFS().Lstat(setupapply.EulaFileName); err == nil {
		t.Fatal("a failed stop must not write any game file")
	}
}

// TestRunSetupApplyReportsStartFailed proves an environment that refuses to
// boot is reported under the start step's own code, distinctly from the
// already-running case above.
func TestRunSetupApplyReportsStartFailed(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	env.startErr = errors.New("setup apply test fixture: refusing to boot")

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if result.Successful || result.ErrorCode != SetupApplyErrorStartFailed {
		t.Fatalf("unexpected result: %+v", result)
	}
	if s.CurrentOperation() != "" {
		t.Fatal("reservation still held after a failed start")
	}
}

// TestRunSetupApplyReportsTimeout proves the attempt's own deadline, rather
// than the step it happened to expire in, decides the reported code. The
// job's bound is overridden here because its configured granularity is
// whole minutes, which no test can wait out.
func TestRunSetupApplyReportsTimeout(t *testing.T) {
	s, client, env := newSetupApplyServer(t)

	previous := setupApplyTimeout
	setupApplyTimeout = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { setupApplyTimeout = previous })

	env.state = environment.ProcessRunningState
	env.stop = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if result.Successful || result.ErrorCode != SetupApplyErrorTimeout {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestRunSetupApplyReportsInternalErrorWhenCancelled proves a server torn
// down mid-attempt ends the job promptly under the internal code rather
// than being mistaken for a deadline that expired.
func TestRunSetupApplyReportsInternalErrorWhenCancelled(t *testing.T) {
	s, client, env := newSetupApplyServer(t)
	env.state = environment.ProcessRunningState
	env.stop = func(ctx context.Context) error {
		s.CtxCancel()
		<-ctx.Done()
		return nil
	}

	req := setupapply.Request{SetupID: uuid.NewString(), Eula: true}
	if _, err := s.AdmitSetupApply(req.SetupID); err != nil {
		t.Fatal(err)
	}

	s.RunSetupApply(req)

	result := <-client.results
	if result.Successful || result.ErrorCode != SetupApplyErrorInternal {
		t.Fatalf("unexpected result: %+v", result)
	}
	select {
	case <-env.started:
		t.Fatal("a cancelled job must not start the server")
	default:
	}
}
