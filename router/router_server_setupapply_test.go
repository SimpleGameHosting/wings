package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
)

const testSetupApplyServerID = "9e3c4f7b-2222-4000-8000-000000000000"

// setupApplyTestEnvironment stops instantly, records starts, and reports
// offline, so the job runs its real apply step without Docker. It also
// answers the environment sync the start step performs, since
// HandlePowerAction(start) reaches Config() and InSituUpdate() on the way
// to actually starting the process.
type setupApplyTestEnvironment struct {
	environment.ProcessEnvironment
	started chan struct{}
	config  *environment.Configuration
}

func (e *setupApplyTestEnvironment) State() string { return environment.ProcessOfflineState }
func (e *setupApplyTestEnvironment) WaitForStop(context.Context, time.Duration, bool) error {
	return nil
}
func (e *setupApplyTestEnvironment) OnBeforeStart(context.Context) error { return nil }
func (e *setupApplyTestEnvironment) Start(context.Context) error {
	select {
	case e.started <- struct{}{}:
	default:
	}
	return nil
}
func (e *setupApplyTestEnvironment) Config() *environment.Configuration { return e.config }
func (e *setupApplyTestEnvironment) InSituUpdate() error                { return nil }

type setupApplyFixture struct {
	manager *wserver.Manager
	results chan remote.SetupApplyResultRequest
}

func newSetupApplyFixture(t *testing.T) *setupApplyFixture {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	next.System.SetupApply.TimeoutMinutes = 1
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	results := make(chan remote.SetupApplyResultRequest, 4)
	return &setupApplyFixture{
		manager: wserver.NewEmptyManager(backupTestRemoteClient{setupResults: results}),
		results: results,
	}
}

func (f *setupApplyFixture) newServer(t *testing.T) *wserver.Server {
	t.Helper()
	s, err := f.manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, testSetupApplyServerID)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	s.Environment = &setupApplyTestEnvironment{
		started: make(chan struct{}, 1),
		config:  environment.NewConfiguration(environment.Settings{}, nil),
	}
	return s
}

func (f *setupApplyFixture) newContext(t *testing.T, s *wserver.Server, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/servers/"+s.ID()+"/setup-apply", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("manager", f.manager)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, recorder
}

func (f *setupApplyFixture) waitResult(t *testing.T) remote.SetupApplyResultRequest {
	t.Helper()
	select {
	case r := <-f.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the setup apply result")
		return remote.SetupApplyResultRequest{}
	}
}

func TestSetupApplyRejectsMalformedPayload(t *testing.T) {
	fixture := newSetupApplyFixture(t)
	s := fixture.newServer(t)
	c, recorder := fixture.newContext(t, s, `{"setup_id":"nope"}`)

	postServerSetupApply(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", recorder.Code, recorder.Body.String())
	}
	if s.ActiveSetupApplyID() != "" {
		t.Fatal("nothing may be admitted after a validation failure")
	}
}

func TestSetupApplyAcceptsRunsAndReports(t *testing.T) {
	fixture := newSetupApplyFixture(t)
	s := fixture.newServer(t)
	setupID := uuid.NewString()
	body := fmt.Sprintf(`{"setup_id":%q,"eula":true,"properties":{"white-list":"true"}}`, setupID)
	c, recorder := fixture.newContext(t, s, body)

	postServerSetupApply(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", recorder.Code, recorder.Body.String())
	}
	var accepted struct {
		SetupID string `json:"setup_id"`
	}
	decodeJSONBody(t, recorder, &accepted)
	if accepted.SetupID != setupID {
		t.Fatalf("202 must echo the setup_id, got %q", accepted.SetupID)
	}
	result := fixture.waitResult(t)
	if !result.Successful || result.SetupID != setupID {
		t.Fatalf("unexpected result %+v", result)
	}
	if _, err := s.Filesystem().UnixFS().Lstat("eula.txt"); err != nil {
		t.Fatalf("eula.txt missing: %v", err)
	}
}

func TestSetupApplyConcurrentDuplicatesBothGet202(t *testing.T) {
	fixture := newSetupApplyFixture(t)
	s := fixture.newServer(t)
	setupID := uuid.NewString()
	body := fmt.Sprintf(`{"setup_id":%q,"eula":true}`, setupID)

	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			c, recorder := fixture.newContext(t, s, body)
			postServerSetupApply(c)
			codes <- recorder.Code
		}()
	}
	first, second := <-codes, <-codes
	if first != http.StatusAccepted || second != http.StatusAccepted {
		t.Fatalf("both duplicates must get 202, got %d and %d", first, second)
	}
	fixture.waitResult(t)
	select {
	case extra := <-fixture.results:
		t.Fatalf("a second job ran: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSetupApplyConflictsWhileAnotherOperationHoldsTheServer(t *testing.T) {
	fixture := newSetupApplyFixture(t)
	s := fixture.newServer(t)
	if err := s.TryBeginOperation(wserver.OperationTransfer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.EndOperation(wserver.OperationTransfer) })

	c, recorder := fixture.newContext(t, s, fmt.Sprintf(`{"setup_id":%q}`, uuid.NewString()))
	postServerSetupApply(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", recorder.Code, recorder.Body.String())
	}
}
