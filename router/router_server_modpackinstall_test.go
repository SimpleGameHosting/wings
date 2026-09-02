package router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// testModpackInstallServerID is the fixed server UUID used by every case in
// this file. Each case builds its own manager and temp filesystem, so reuse
// across cases is safe the same way it already is in the neighboring
// fingerprint and power router tests.
const testModpackInstallServerID = "8d2b3f6a-1111-4000-8000-000000000000"

// modpackInstallStopDelay is how long modpackInstallTestEnvironment waits
// before reporting its stop failure. It exists to keep the job's active-id
// window observable: this sandbox has no reachable Docker daemon, so the
// pipeline's real "stopping" stage would otherwise fail in well under a
// millisecond, faster than any poll loop could reliably catch.
const modpackInstallStopDelay = 100 * time.Millisecond

// modpackInstallTestEnvironment stands in for a server's real Docker
// environment so a case can control exactly how long the pipeline's
// "stopping" stage takes before it fails, rather than depending on the
// timing of a real (and here, near-instant) Docker connection failure. It
// deliberately always fails, the same outcome a real environment would
// reach with no daemon to talk to, so the job still terminates through its
// ordinary error path and clears its active id.
type modpackInstallTestEnvironment struct {
	environment.ProcessEnvironment
}

// WaitForStop waits out modpackInstallStopDelay, or the context, whichever
// comes first, and then reports a stop failure.
func (e *modpackInstallTestEnvironment) WaitForStop(ctx context.Context, _ time.Duration, _ bool) error {
	select {
	case <-time.After(modpackInstallStopDelay):
	case <-ctx.Done():
	}
	return errors.New("modpack install test fixture: no environment to stop")
}

// modpackInstallFixture bundles what every case in this file needs: an
// isolated global config with a known, small install-slot cap so slot
// exhaustion is deterministic, and a manager that request contexts are
// wired against.
type modpackInstallFixture struct {
	manager *wserver.Manager
}

// newModpackInstallFixture pins the node-wide install cap to maxConcurrent
// for the lifetime of the calling test, restoring the previous global
// config on cleanup. The cap must be set explicitly rather than trusted
// from ambient config, since the package's shared test init assigns a bare
// config.Configuration without its default tags, leaving it at the zero
// value.
func newModpackInstallFixture(t *testing.T, maxConcurrent int) *modpackInstallFixture {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	next.System.ModpackInstall.MaxConcurrent = maxConcurrent
	next.System.ModpackInstall.TimeoutMinutes = 1
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	return &modpackInstallFixture{manager: wserver.NewEmptyManager(backupTestRemoteClient{})}
}

// newServer builds a server tracked by the fixture's manager, cancelling its
// context on test cleanup the same way the fingerprint and power fixtures
// do.
func (f *modpackInstallFixture) newServer(t *testing.T, id string) *wserver.Server {
	t.Helper()

	s, err := f.manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, id)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)

	// Swap in a fixture environment with a controlled, observable stop
	// delay in place of the real Docker environment InitServer wired up,
	// the same way the power router tests take control of the power lock...
	s.Environment = &modpackInstallTestEnvironment{}

	return s
}

// newContext builds a gin request context matching the Panel's modpack
// install request path, with the given server and this fixture's manager
// injected the same way the fingerprint and power fixtures inject theirs.
func (f *modpackInstallFixture) newContext(t *testing.T, s *wserver.Server, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/servers/"+s.ID()+"/modpack-install", strings.NewReader(body))
	c.Request = request
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("manager", f.manager)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, recorder
}

// newModpackArchiveServer starts an httptest server serving a tiny, valid
// gzip-compressed tar archive with an explicit Content-Length, standing in
// for the Panel's signed download URL so a case that reaches the download
// stage of the pipeline has a real artifact to fetch.
func newModpackArchiveServer(t *testing.T) *httptest.Server {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("modpack-fixture")
	if err := tw.WriteHeader(&tar.Header{Name: "server.properties", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// validModpackInstallBody builds a request body that passes
// modpackinstall.Request.Validate for a "modpack" kind install.
func validModpackInstallBody(installID, downloadURL string) string {
	return fmt.Sprintf(`{"install_id":%q,"kind":"modpack","download_url":%q}`, installID, downloadURL)
}

// waitForActiveModpackInstallID polls the server's active install id until
// it equals want or timeout elapses. Polling, rather than a single read
// right after the handler returns, is required because the job goroutine
// records its own id as the very first thing it does, and that goroutine's
// scheduling is never synchronized with the handler returning its 202.
func waitForActiveModpackInstallID(t *testing.T, s *wserver.Server, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for s.ActiveModpackInstallID() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the active modpack install id to become %q, last saw %q", want, s.ActiveModpackInstallID())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// decodeJSONBody unmarshals a recorder's body into v, failing the test on
// any decode error so every case below can assert on typed fields instead
// of raw strings.
func decodeJSONBody(t *testing.T, recorder *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), v); err != nil {
		t.Fatalf("failed to decode response body %q: %v", recorder.Body.String(), err)
	}
}

// TestModpackInstallInvalidPayloadReturns400 covers case 1: a syntactically
// valid request whose kind fails semantic validation must be rejected
// before anything is claimed.
func TestModpackInstallInvalidPayloadReturns400(t *testing.T) {
	fixture := newModpackInstallFixture(t, 2)
	s := fixture.newServer(t, testModpackInstallServerID)

	body := fmt.Sprintf(`{"install_id":%q,"kind":"bogus","download_url":"http://example.com/pack.tar.gz"}`, uuid.NewString())
	c, recorder := fixture.newContext(t, s, body)

	postServerModpackInstall(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected an invalid kind to return 400, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	decodeJSONBody(t, recorder, &result)
	if !strings.Contains(result.Error, "kind") {
		t.Fatalf("expected the error message to mention the invalid kind, got %q", result.Error)
	}
	if id := s.ActiveModpackInstallID(); id != "" {
		t.Fatalf("expected no install to become active after a validation failure, got %q", id)
	}
}

// TestModpackInstallAcceptsValidRequestAndTracksLifecycle covers case 2: a
// valid request against an idle server is accepted, the install_id becomes
// active immediately, and it clears again once the job finishes.
func TestModpackInstallAcceptsValidRequestAndTracksLifecycle(t *testing.T) {
	fixture := newModpackInstallFixture(t, 2)
	s := fixture.newServer(t, testModpackInstallServerID)
	archive := newModpackArchiveServer(t)

	installID := uuid.NewString()
	c, recorder := fixture.newContext(t, s, validModpackInstallBody(installID, archive.URL))

	postServerModpackInstall(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected a valid request against an idle server to return 202, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		InstallID string `json:"install_id"`
	}
	decodeJSONBody(t, recorder, &result)
	if result.InstallID != installID {
		t.Fatalf("expected response install_id %q, got %q", installID, result.InstallID)
	}

	waitForActiveModpackInstallID(t, s, installID, 2*time.Second)
	waitForActiveModpackInstallID(t, s, "", 5*time.Second)
}

// TestModpackInstallRepeatOfActiveInstallReturns202WithoutStartingAnother
// covers case 3: repeating the exact install_id that is already running
// must be answered with the same 202 and must not consume a second slot.
func TestModpackInstallRepeatOfActiveInstallReturns202WithoutStartingAnother(t *testing.T) {
	fixture := newModpackInstallFixture(t, 2)
	s := fixture.newServer(t, testModpackInstallServerID)
	archive := newModpackArchiveServer(t)

	installID := uuid.NewString()
	body := validModpackInstallBody(installID, archive.URL)

	first, firstRecorder := fixture.newContext(t, s, body)
	postServerModpackInstall(first)
	if firstRecorder.Code != http.StatusAccepted {
		t.Fatalf("expected the first attempt to return 202, got %d body %s", firstRecorder.Code, firstRecorder.Body.String())
	}

	// The job goroutine records its own id as its first statement, so wait
	// for that to happen before firing the duplicate; otherwise the repeat
	// can race the goroutine and see no active id yet...
	waitForActiveModpackInstallID(t, s, installID, 2*time.Second)

	second, secondRecorder := fixture.newContext(t, s, body)
	postServerModpackInstall(second)
	if secondRecorder.Code != http.StatusAccepted {
		t.Fatalf("expected a repeat of the active install id to return 202, got %d body %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	var result struct {
		InstallID string `json:"install_id"`
	}
	decodeJSONBody(t, secondRecorder, &result)
	if result.InstallID != installID {
		t.Fatalf("expected the repeat response install_id %q, got %q", installID, result.InstallID)
	}

	// If the repeat had wrongly started a second job, it would have taken
	// the node's other slot; confirm it is still free...
	release, ok := fixture.manager.TryReserveModpackInstallSlot()
	if !ok {
		t.Fatal("expected the repeat request to not have consumed a second install slot")
	}
	release()

	waitForActiveModpackInstallID(t, s, "", 5*time.Second)
}

// TestModpackInstallDifferentInstallIDWhileActiveReturns409 covers case 4:
// a different install_id arriving while one is already active must be
// refused as a conflict rather than queued or silently dropped.
func TestModpackInstallDifferentInstallIDWhileActiveReturns409(t *testing.T) {
	fixture := newModpackInstallFixture(t, 2)
	s := fixture.newServer(t, testModpackInstallServerID)
	archive := newModpackArchiveServer(t)

	activeID := uuid.NewString()
	first, firstRecorder := fixture.newContext(t, s, validModpackInstallBody(activeID, archive.URL))
	postServerModpackInstall(first)
	if firstRecorder.Code != http.StatusAccepted {
		t.Fatalf("expected the first attempt to return 202, got %d body %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	waitForActiveModpackInstallID(t, s, activeID, 2*time.Second)

	c, recorder := fixture.newContext(t, s, validModpackInstallBody(uuid.NewString(), archive.URL))
	postServerModpackInstall(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected a different install id while one is active to return 409, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, recorder, &result)
	if result.Error != "An installation is already in progress for this server." {
		t.Fatalf("unexpected conflict message %q", result.Error)
	}

	waitForActiveModpackInstallID(t, s, "", 5*time.Second)
}

// TestModpackInstallConflictsWithActiveTransfer covers case 5: a server
// already claimed by another exclusive operation, a transfer here, must
// refuse the install with a message naming that operation.
func TestModpackInstallConflictsWithActiveTransfer(t *testing.T) {
	fixture := newModpackInstallFixture(t, 2)
	s := fixture.newServer(t, testModpackInstallServerID)

	if err := s.TryBeginOperation(wserver.OperationTransfer); err != nil {
		t.Fatalf("failed to claim a fake transfer for the fixture: %v", err)
	}
	t.Cleanup(func() { s.EndOperation(wserver.OperationTransfer) })

	c, recorder := fixture.newContext(t, s, validModpackInstallBody(uuid.NewString(), "http://example.com/pack.tar.gz"))
	postServerModpackInstall(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected a server already transferring to return 409, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, recorder, &result)
	if result.Error != "A transfer is already in progress for this server." {
		t.Fatalf("unexpected conflict message %q", result.Error)
	}
	if id := s.ActiveModpackInstallID(); id != "" {
		t.Fatalf("expected no install to become active while a transfer holds the server, got %q", id)
	}
}

// TestModpackInstallSlotExhaustionReturns409AndReleasesOnFailure covers
// case 6: once every node-wide install slot is taken, a new request must be
// refused, and the per-server operation claim it made along the way must be
// released so a follow-up attempt, after a slot frees up, can still
// succeed.
func TestModpackInstallSlotExhaustionReturns409AndReleasesOnFailure(t *testing.T) {
	const maxConcurrent = 2
	fixture := newModpackInstallFixture(t, maxConcurrent)
	s := fixture.newServer(t, testModpackInstallServerID)
	archive := newModpackArchiveServer(t)

	var releases []func()
	for i := 0; i < maxConcurrent; i++ {
		release, ok := fixture.manager.TryReserveModpackInstallSlot()
		if !ok {
			t.Fatalf("expected slot %d of %d to be reservable while priming exhaustion", i, maxConcurrent)
		}
		releases = append(releases, release)
	}

	body := validModpackInstallBody(uuid.NewString(), archive.URL)
	c, recorder := fixture.newContext(t, s, body)
	postServerModpackInstall(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected an exhausted node to return 409, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, recorder, &result)
	if result.Error != "this node is running its maximum number of concurrent installs, try again shortly" {
		t.Fatalf("unexpected conflict message %q", result.Error)
	}

	// The operation claim made before the slot check failed must have been
	// released, or this server would be stuck refusing every future
	// attempt even after node capacity frees up...
	if current := s.CurrentOperation(); current != "" {
		t.Fatalf("expected the operation reservation to be released after a slot conflict, got %q", current)
	}

	releases[0]()

	retry, retryRecorder := fixture.newContext(t, s, body)
	postServerModpackInstall(retry)
	if retryRecorder.Code != http.StatusAccepted {
		t.Fatalf("expected a follow-up attempt after a slot frees up to succeed, got %d body %s", retryRecorder.Code, retryRecorder.Body.String())
	}

	waitForActiveModpackInstallID(t, s, "", 5*time.Second)
	releases[1]()
}
