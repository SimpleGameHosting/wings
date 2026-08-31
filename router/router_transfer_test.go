package router

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/transfer"
)

// transferTestRemoteClient records transfer status calls and can simulate a
// panel that is unreachable.
type transferTestRemoteClient struct {
	remote.Client

	mu        sync.Mutex
	statusErr error
	statusSet []bool
}

func (c *transferTestRemoteClient) SetTransferStatus(_ context.Context, _ string, successful bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusSet = append(c.statusSet, successful)
	return c.statusErr
}

// statuses returns a copy of the recorded transfer status calls.
func (c *transferTestRemoteClient) statuses() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.statusSet...)
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
	s.SetTransferring(true)

	leftoverContents := "partial data"
	if err := s.Filesystem().Write("leftover.txt", strings.NewReader(leftoverContents), int64(len(leftoverContents)), 0o644); err != nil {
		t.Fatal(err)
	}

	trnsfr := transfer.New(context.Background(), s)
	transfer.Incoming().Add(trnsfr)
	t.Cleanup(func() { transfer.Incoming().Remove(trnsfr) })

	return manager, trnsfr
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
