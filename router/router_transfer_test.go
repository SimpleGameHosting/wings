package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/router/tokens"
	wserver "github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/transfer"
)

// transferTestRemoteClient records transfer status calls and can simulate a
// panel that is unreachable.
type transferTestRemoteClient struct {
	remote.Client

	mu               sync.Mutex
	statusErr        error
	configurationErr error
	statusSet        []bool
	statusServers    []string
}

func (c *transferTestRemoteClient) SetTransferStatus(_ context.Context, uuid string, successful bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusSet = append(c.statusSet, successful)
	c.statusServers = append(c.statusServers, uuid)
	return c.statusErr
}

// GetServerConfiguration simulates the panel configuration lookup a brand-new
// incoming transfer performs. Tests opt in by setting configurationErr; any
// other call is unexpected and fails loudly.
func (c *transferTestRemoteClient) GetServerConfiguration(_ context.Context, _ string) (remote.ServerConfigurationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.configurationErr == nil {
		panic("transferTestRemoteClient: unexpected GetServerConfiguration call")
	}

	return remote.ServerConfigurationResponse{}, c.configurationErr
}

// statuses returns a copy of the recorded transfer status calls.
func (c *transferTestRemoteClient) statuses() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.statusSet...)
}

// statusServerIDs returns a copy of the server UUIDs the recorded transfer
// status calls were made for.
func (c *transferTestRemoteClient) statusServerIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statusServers...)
}

// newTransferFixture builds a manager-registered server with a real filesystem
// and an incoming transfer wrapping it, mirroring the state postTransfers
// leaves behind right before its deferred cleanup runs.
func newTransferFixture(t *testing.T, client *transferTestRemoteClient) (*wserver.Manager, *transfer.Transfer) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	manager := wserver.NewEmptyManager(client)
	s, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"8d2b3f6a-0000-4000-8000-000000000000"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	manager.Add(s)
	if err := s.TryBeginOperation(wserver.OperationTransfer); err != nil {
		t.Fatal(err)
	}

	leftoverContents := "partial data"
	if err := s.Filesystem().Write("leftover.txt", strings.NewReader(leftoverContents), int64(len(leftoverContents)), 0o644); err != nil {
		t.Fatal(err)
	}

	trnsfr := transfer.New(context.Background(), s)
	transfer.Incoming().Add(trnsfr)
	t.Cleanup(func() { transfer.Incoming().Remove(trnsfr) })

	return manager, trnsfr
}

// signTransferToken builds a signed transfer JWT for the given server UUID,
// mirroring what the panel issues to the source node.
func signTransferToken(t *testing.T, serverUUID string) string {
	t.Helper()

	payload := tokens.TransferPayload{
		Payload: jwt.Payload{
			Subject:        serverUUID,
			ExpirationTime: jwt.NumericDate(time.Now().Add(10 * time.Minute)),
		},
		Scoped: tokens.Scoped{Scope: string(tokens.ServerTransfer)},
	}
	signed, err := jwt.Sign(&payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

// newTransferRequestContext builds a POST /api/transfers context carrying the
// signed token and the manager, exactly as the router middleware would.
func newTransferRequestContext(t *testing.T, manager *wserver.Manager, token string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/transfers", strings.NewReader(""))
	c.Request = request
	c.Request.Header.Set("Authorization", "Bearer "+token)
	c.Set("manager", manager)
	return c, recorder
}

// TestPostTransfersRejectsDuplicateInFlightTransfer ensures a second POST for
// a server that is already transferring is rejected outright: it must never
// join the live transfer, because its failure cleanup would delete the files
// the original request is still extracting.
func TestPostTransfersRejectsDuplicateInFlightTransfer(t *testing.T) {
	client := &transferTestRemoteClient{}
	manager, trnsfr := newTransferFixture(t, client)
	dataPath := trnsfr.Server.Filesystem().Path()
	c, recorder := newTransferRequestContext(t, manager, signTransferToken(t, trnsfr.Server.ID()))

	postTransfers(c)

	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected duplicate transfer request to return 409, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("expected the in-flight transfer files to be untouched, stat err: %v", err)
	}
	if transfer.Incoming().Get(trnsfr.Server.ID()) == nil {
		t.Fatal("expected the in-flight transfer to remain registered")
	}
	if !trnsfr.Server.IsTransferring() {
		t.Fatal("expected the in-flight transfer to keep its transferring flag")
	}
}

// TestPostTransfersRejectsServerAlreadyOnNode ensures a replayed transfer for
// a server this node already hosts is rejected before any transfer state is
// created, so the failure cleanup can never delete a live server's data.
func TestPostTransfersRejectsServerAlreadyOnNode(t *testing.T) {
	client := &transferTestRemoteClient{}
	manager, trnsfr := newTransferFixture(t, client)
	transfer.Incoming().Remove(trnsfr)
	trnsfr.Server.EndOperation(wserver.OperationTransfer)
	dataPath := trnsfr.Server.Filesystem().Path()
	c, recorder := newTransferRequestContext(t, manager, signTransferToken(t, trnsfr.Server.ID()))

	postTransfers(c)

	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected replayed transfer request to return 409, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("expected the live server files to be untouched, stat err: %v", err)
	}
	if _, ok := manager.Get(trnsfr.Server.ID()); !ok {
		t.Fatal("expected the live server to remain in the manager")
	}

	// The rejected request must release its reservation on the way out so a
	// later legitimate transfer attempt is not locked out...
	if !transfer.Incoming().TryReserve(trnsfr.Server.ID()) {
		t.Fatal("expected the reservation to be released after the rejected request returned")
	}
	transfer.Incoming().Release(trnsfr.Server.ID())
}

// TestIncomingTransferReservations covers the atomic reservation that closes
// the concurrent-duplicate window: only one request may hold a server's
// reservation, a registered transfer blocks new reservations, and releasing
// is idempotent so callers can release unconditionally on exit.
func TestIncomingTransferReservations(t *testing.T) {
	const reservationID = "11111111-2222-4333-8444-555555555555"

	if !transfer.Incoming().TryReserve(reservationID) {
		t.Fatal("expected the first reservation to succeed")
	}
	t.Cleanup(func() { transfer.Incoming().Release(reservationID) })
	if transfer.Incoming().TryReserve(reservationID) {
		t.Fatal("expected a duplicate reservation to fail while held")
	}

	transfer.Incoming().Release(reservationID)
	if !transfer.Incoming().TryReserve(reservationID) {
		t.Fatal("expected a reservation after release to succeed")
	}
	transfer.Incoming().Release(reservationID)

	// Releasing an id that was never reserved must be a safe no-op...
	transfer.Incoming().Release("never-reserved")

	// A registered in-flight transfer must block reservations even when no
	// explicit reservation is held, so handler fixtures and restarts of the
	// daemon state cannot be raced...
	client := &transferTestRemoteClient{}
	_, trnsfr := newTransferFixture(t, client)
	if transfer.Incoming().TryReserve(trnsfr.Server.ID()) {
		t.Fatal("expected the reservation to fail while a transfer is registered")
	}
}

// TestFinalizeFailedTransferDeletesFilesWhenPanelReachable covers the disk
// leak from pterodactyl/panel#5555: a failed transfer whose status call
// succeeds must still remove the extracted files from this node.
func TestFinalizeFailedTransferDeletesFilesWhenPanelReachable(t *testing.T) {
	client := &transferTestRemoteClient{}
	manager, trnsfr := newTransferFixture(t, client)
	dataPath := trnsfr.Server.Filesystem().Path()

	finalizeIncomingTransfer(manager, trnsfr, false)

	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("expected failed transfer files at %s to be deleted, stat err: %v", dataPath, err)
	}
	if transfer.Incoming().Get(trnsfr.Server.ID()) != nil {
		t.Fatal("expected transfer to be removed from the incoming registry")
	}
	if _, ok := manager.Get(trnsfr.Server.ID()); ok {
		t.Fatal("expected the server to be removed from the manager after a failed transfer")
	}
	if got := client.statuses(); len(got) != 1 || got[0] != false {
		t.Fatalf("expected one failed status call, got %v", got)
	}
	if trnsfr.Server.IsTransferring() {
		t.Fatal("expected the server to no longer be marked as transferring")
	}
}

// TestFinalizeFailedTransferDeletesFilesWhenPanelUnreachable verifies the
// cleanup no longer depends on the panel status call failing or succeeding,
// and that an unreachable panel cannot strand the transferring flag.
func TestFinalizeFailedTransferDeletesFilesWhenPanelUnreachable(t *testing.T) {
	client := &transferTestRemoteClient{statusErr: context.DeadlineExceeded}
	manager, trnsfr := newTransferFixture(t, client)
	dataPath := trnsfr.Server.Filesystem().Path()

	finalizeIncomingTransfer(manager, trnsfr, false)

	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("expected files to be deleted even when the panel is unreachable, stat err: %v", err)
	}
	if trnsfr.Server.IsTransferring() {
		t.Fatal("expected transferring flag to clear even when the panel status call fails")
	}
}

// TestPostTransfersReportsFailureWhenInstallerCreationFails drives a new
// incoming transfer through the real router with a valid transfer token while
// the panel cannot serve the server configuration. The handler must report
// the failed transfer to the panel using the UUID from the token subject:
// upstream dereferenced the transfer's not-yet-assigned server here, so the
// handler panicked and the panel never learned the transfer failed.
func TestPostTransfersReportsFailureWhenInstallerCreationFails(t *testing.T) {
	client := &transferTestRemoteClient{configurationErr: errors.New("panel is unreachable")}
	manager := wserver.NewEmptyManager(client)
	engine := Configure(manager, client)

	// Sign a transfer token for a server this node has never seen, exactly
	// like the panel does when it initiates a transfer to a new node...
	serverID := "0a4f26a6-4c84-46b4-9e5d-6f68e4f0a1c9"
	signed, err := jwt.Sign(&tokens.TransferPayload{
		Payload: jwt.Payload{
			Subject:        serverID,
			ExpirationTime: jwt.NumericDate(time.Now().Add(10 * time.Minute)),
		},
		Scoped: tokens.Scoped{Scope: string(tokens.ServerTransfer)},
	}, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/transfers", nil)
	req.Header.Set("Authorization", "Bearer "+string(signed))
	res := httptest.NewRecorder()

	engine.ServeHTTP(res, req)

	if got := client.statuses(); len(got) != 1 || got[0] {
		t.Fatalf("expected exactly one failed transfer status call to the panel, got %v", got)
	}
	if got := client.statusServerIDs(); len(got) != 1 || got[0] != serverID {
		t.Fatalf("expected the failed status call to use the token subject %q, got %v", serverID, got)
	}
	if res.Code != http.StatusInternalServerError || !strings.Contains(res.Body.String(), "error") {
		t.Fatalf("expected a handled error response, got status %d with body %q", res.Code, res.Body.String())
	}
}

// TestFinalizeSuccessfulTransferKeepsFiles ensures the cleanup is strictly
// limited to failed transfers so a completed transfer cannot lose data.
func TestFinalizeSuccessfulTransferKeepsFiles(t *testing.T) {
	client := &transferTestRemoteClient{}
	manager, trnsfr := newTransferFixture(t, client)
	dataPath := trnsfr.Server.Filesystem().Path()

	finalizeIncomingTransfer(manager, trnsfr, true)

	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("expected successful transfer files to remain, stat err: %v", err)
	}
	if _, ok := manager.Get(trnsfr.Server.ID()); !ok {
		t.Fatal("expected the server to remain in the manager after a successful transfer")
	}
	if got := client.statuses(); len(got) != 1 || got[0] != true {
		t.Fatalf("expected one successful status call, got %v", got)
	}
	if trnsfr.Server.IsTransferring() {
		t.Fatal("expected the server to no longer be marked as transferring")
	}
}
