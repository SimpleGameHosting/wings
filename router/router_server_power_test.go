package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
)

// blockingTestEnvironment satisfies environment.ProcessEnvironment through
// embedding and overrides only what the stop and kill paths touch, so a test
// can hold the server's power lock exactly the way a real boot or stop does.
type blockingTestEnvironment struct {
	environment.ProcessEnvironment

	release chan struct{}
}

// WaitForStop blocks until the test releases it, keeping the power lock held.
func (e *blockingTestEnvironment) WaitForStop(ctx context.Context, _ time.Duration, _ bool) error {
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil
}

// Terminate is a no-op so a kill request routed through the handler cannot
// panic on the embedded nil interface.
func (e *blockingTestEnvironment) Terminate(_ context.Context, _ string) error {
	return nil
}

// powerReleaseOnce guards double-closing the release channel between test body
// and cleanup.
var powerReleaseOnce sync.Mutex

// newPowerFixture returns a request context for the power endpoint whose
// server is busy processing a fake power action that holds the lock.
func newPowerFixture(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder, *wserver.Server, func()) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	manager := wserver.NewEmptyManager(backupTestRemoteClient{})
	s, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"8d2b3f6a-0000-4000-8000-000000000000"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)

	release := make(chan struct{})
	releaseOnce := func() {
		powerReleaseOnce.Lock()
		defer powerReleaseOnce.Unlock()
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(releaseOnce)
	s.Environment = &blockingTestEnvironment{release: release}

	// Occupy the power lock the same way a slow stop or boot would...
	go func() { _ = s.HandlePowerAction(wserver.PowerActionStop) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.ExecutingPowerAction() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the fake power action to hold the lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/servers/8d2b3f6a-0000-4000-8000-000000000000/power", strings.NewReader(body))
	c.Request = request
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, recorder, s, releaseOnce
}

// TestPostServerPowerConflictsWhileAnotherActionRuns covers the silent drop
// from pterodactyl/panel#5712: a stop sent during a boot returned 202 and then
// vanished when the lock acquisition failed in the background.
func TestPostServerPowerConflictsWhileAnotherActionRuns(t *testing.T) {
	c, recorder, _, _ := newPowerFixture(t, `{"action":"stop"}`)

	postServerPower(c)

	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected stop during an executing power action to return 409, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}

// TestPostServerPowerStillAcceptsKillWhileLocked ensures the conflict check
// never blocks termination, which is the escape hatch for stuck servers.
func TestPostServerPowerStillAcceptsKillWhileLocked(t *testing.T) {
	c, recorder, _, _ := newPowerFixture(t, `{"action":"kill"}`)

	postServerPower(c)

	if c.Writer.Status() != http.StatusAccepted {
		t.Fatalf("expected kill to bypass the conflict check, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}

// TestPostServerPowerStillQueuesExplicitWaits ensures callers that opt into
// waiting on the lock with wait_seconds keep their queueing behavior.
func TestPostServerPowerStillQueuesExplicitWaits(t *testing.T) {
	c, recorder, _, _ := newPowerFixture(t, `{"action":"stop","wait_seconds":10}`)

	postServerPower(c)

	if c.Writer.Status() != http.StatusAccepted {
		t.Fatalf("expected an explicit wait_seconds request to be accepted, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}
