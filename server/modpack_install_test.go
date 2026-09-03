package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/modpackinstall"
	"github.com/pterodactyl/wings/remote"
)

// ensureModpackInstallSlotDefaults pins the node-wide install cap to its
// documented default (3) regardless of what an earlier test in this package
// left in the global configuration. Several tests elsewhere in this package
// call config.Set with a bare Configuration literal, which replaces the
// whole global struct without reapplying the "default" struct tags, so a
// test that depends on config.Get() still holding Task 7's defaults cannot
// simply trust ambient state left by whatever ran before it.
func ensureModpackInstallSlotDefaults(t *testing.T) {
	t.Helper()
	config.Update(func(c *config.Configuration) {
		c.System.ModpackInstall.MaxConcurrent = 3
	})
}

// TestModpackInstallSlotCap locks down the node-wide concurrency ceiling: a
// reservation is granted below the cap, refused once it is reached, and a
// released slot becomes available again through an idempotent release func.
func TestModpackInstallSlotCap(t *testing.T) {
	ensureModpackInstallSlotDefaults(t)
	m := NewEmptyManager(nil)
	// Default cap is 3.
	var releases []func()
	for i := 0; i < 3; i++ {
		rel, ok := m.TryReserveModpackInstallSlot()
		if !ok {
			t.Fatalf("slot %d refused below cap", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := m.TryReserveModpackInstallSlot(); ok {
		t.Fatal("4th slot must be refused")
	}
	releases[0]()
	releases[0]() // double release must be safe
	if _, ok := m.TryReserveModpackInstallSlot(); !ok {
		t.Fatal("slot must free after release")
	}
}

// TestModpackInstallSlotConcurrency hammers the reservation from 32 parallel
// goroutines and checks the mutex-guarded counter never grants more than the
// configured cap, the property a bare unsynchronized counter would violate
// under -race.
func TestModpackInstallSlotConcurrency(t *testing.T) {
	ensureModpackInstallSlotDefaults(t)
	m := NewEmptyManager(nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := m.TryReserveModpackInstallSlot(); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 3 {
		t.Errorf("granted=%d want exactly 3", granted)
	}
}

// modpackInstallTestClient records the context a finished install reported
// its outcome with, observed at call time.
type modpackInstallTestClient struct {
	remote.Client

	reports chan fencedReportContext
}

func (c modpackInstallTestClient) SendModpackInstallResult(ctx context.Context, _ string, _ remote.ModpackInstallResultRequest) error {
	c.reports <- observeFencedReportContext(ctx)
	return nil
}

// newModpackInstallServer builds a server whose panel client only records
// the install result callback, which is all the finisher needs.
func newModpackInstallServer(t *testing.T) (*Server, modpackInstallTestClient) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	settings := json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, uuid.NewString()))
	client := modpackInstallTestClient{reports: make(chan fencedReportContext, 1)}
	s, err := NewEmptyManager(client).InitServer(setupApplyServerConfiguration(settings))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	return s, client
}

// TestFinishModpackInstallReportsTheResultWithABoundedContext checks the
// panel callback carries a deadline of its own instead of the attempt's
// context, which on the timeout path is already expired when the report is
// sent.
func TestFinishModpackInstallReportsTheResultWithABoundedContext(t *testing.T) {
	s, client := newModpackInstallServer(t)

	s.finishModpackInstall(modpackinstall.Request{InstallID: uuid.NewString()}, nil, time.Now(), func() {})

	report := <-client.reports
	if !report.bounded {
		t.Fatal("the result report must be sent with a bounded context")
	}
	if report.err != nil {
		t.Fatalf("the result report context was already done: %v", report.err)
	}
}
