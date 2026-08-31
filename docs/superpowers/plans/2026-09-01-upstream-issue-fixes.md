# Upstream Issue Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix four validated upstream Pterodactyl bugs in the SGH wings fork: failed-transfer disk leaks, symlink loss during extraction and restore, silently ignored power actions during boot, and synchronous decompression timeouts.

**Architecture:** Each fix is an independent, additive patch on the `sgh` patch series, implemented test-first and committed separately with its own SGH-PATCHES.md registry entry.
The transfer fix extracts the deferred cleanup into a testable function.
The symlink fix threads `archives.FileInfo` through the restore callback so extraction learns link targets.
The power fix adds an honest 409 at the router plus a docker-environment guard that stops a booting server from being falsely marked offline.
The decompress fix moves the space walk and extraction into a background goroutine with daemon console feedback.

**Tech Stack:** Go 1.25 (container-only builds: wings does not compile on macOS), testify + goblin tests, mholt/archives, docker SDK.

**Spec:** The validated findings live in the conversation record summarized below; upstream references are panel issues #5555, #5429, #5712, #2878 and wings PRs #298 and #286 (diffs in the session scratchpad).

## Background (validated findings)

1. `router/router_transfer.go:108` only deletes a failed transfer's extracted files when `SetTransferStatus` to the panel ALSO fails, and the early `return` on that error path skips `SetTransferring(false)`.
   Normal failed transfers therefore orphan the entire data directory forever (server already removed from the manager).
2. Archive creation preserves symlinks (SGH fingerprint patch), but `extractStream` (`server/filesystem/compress.go:277`) and `restoreBackupEntry` (`server/backup.go:160`) have no symlink branch: `Filesystem.Write` materializes each symlink entry as an EMPTY REGULAR FILE during transfers, decompression, and backup restore.
3. `postServerPower` (`router/router_server.go:53`) returns 202 then drops stop/restart silently when the boot goroutine holds the power lock (panel sends `wait_seconds=0`).
   Kill bypasses the lock and `SignalContainer`/`Terminate` (`environment/docker/power.go:291,322`) force the state to offline while `Start` is still running, desyncing the panel and poking crash detection.
4. `postServerDecompressFiles` (`router/router_server_files.go:455`) runs the full-archive space walk plus extraction synchronously in the HTTP handler; multi-GB archives 504 at the proxy while extraction continues invisibly.

## Global Constraints

- All build/test commands run in a Linux container from the worktree root:
  `docker run --rm -v /Users/kane/Development/simplegamehosting/sgh_projects/wings/.claude/worktrees/upstream-issue-fixes:/src -w /src -v wings-sgh-gomod:/go/pkg/mod -v wings-sgh-gocache:/root/.cache/go-build golang:1.25 <cmd>`
- Verification gate before each commit: `gofmt -l .` (empty), `go vet ./...`, `go test ./...` (targeted package during TDD, full suite before commit).
- Final gate after Task 4: `CGO_ENABLED=1 go test -race ./...` in the same container.
- Every fix updates SGH-PATCHES.md (What/Why/Files/Conflict-risk entry, one sentence per line) inside the same commit.
- Commit messages: conventional prefix (`fix(...)`, `feat(...)`), no co-author trailers, never mention agents.
- Code style: doc block above every new function stating intent in plain sentences; full-sentence step comments ending in `...` only where non-obvious; guard clauses; no em dashes anywhere.
- Do not touch resumable-upload pilot surfaces (`router_server_upload*.go`, `server/uploads.go`); this branch is based on `sgh`, which does not contain them.

---

### Task 1: Failed-transfer cleanup (panel#5555)

**Files:**
- Modify: `router/router_transfer.go:96-127` (extract deferred body into `finalizeIncomingTransfer`)
- Create: `router/router_transfer_test.go`
- Modify: `SGH-PATCHES.md`

**Interfaces:**
- Consumes: `transfer.Incoming()`, `server.Manager.Remove`, `remote.Client.SetTransferStatus`, `server.TransferStatusEvent`.
- Produces: `finalizeIncomingTransfer(manager *server.Manager, trnsfr *transfer.Transfer, successful bool)` in package `router`, called from the deferred func in `postTransfers`.

- [ ] **Step 1: Write the failing tests** in `router/router_transfer_test.go`:

```go
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

	mu         sync.Mutex
	statusErr  error
	statusUUID string
	statusSet  []bool
}

func (c *transferTestRemoteClient) SetTransferStatus(_ context.Context, uuid string, successful bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusUUID = uuid
	c.statusSet = append(c.statusSet, successful)
	return c.statusErr
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

	if err := s.Filesystem().Write("leftover.txt", strings.NewReader("partial data"), 12, 0o644); err != nil {
		t.Fatal(err)
	}

	trnsfr := transfer.New(context.Background(), s)
	transfer.Incoming().Add(trnsfr)
	return manager, trnsfr
}

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
	if len(client.statusSet) != 1 || client.statusSet[0] != false {
		t.Fatalf("expected one failed status call, got %v", client.statusSet)
	}
	if trnsfr.Server.IsTransferring() {
		t.Fatal("expected the server to no longer be marked as transferring")
	}
}

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

func TestFinalizeSuccessfulTransferKeepsFiles(t *testing.T) {
	client := &transferTestRemoteClient{}
	manager, trnsfr := newTransferFixture(t, client)
	dataPath := trnsfr.Server.Filesystem().Path()

	finalizeIncomingTransfer(manager, trnsfr, true)

	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("expected successful transfer files to remain, stat err: %v", err)
	}
	if len(client.statusSet) != 1 || client.statusSet[0] != true {
		t.Fatalf("expected one successful status call, got %v", client.statusSet)
	}
	if trnsfr.Server.IsTransferring() {
		t.Fatal("expected the server to no longer be marked as transferring")
	}
}
```

Adjust fixture details to reality (e.g. `manager.Add` signature, `Filesystem().Write` length) if the compiler disagrees; the assertions are the contract.

- [ ] **Step 2: Run to verify failure**: package `./router` test run fails with `undefined: finalizeIncomingTransfer`.

- [ ] **Step 3: Implement** in `router/router_transfer.go`: replace the deferred anonymous body with a call to the new function; delete files synchronously BEFORE notifying the panel so the panel cannot retry into a directory that is still being deleted:

```go
// finalizeIncomingTransfer settles an incoming transfer once the request body
// has been fully processed. A failed transfer must always drop the server and
// its extracted files from this node, and the panel must be notified last so
// a retry cannot race the cleanup.
func finalizeIncomingTransfer(manager *server.Manager, trnsfr *transfer.Transfer, successful bool) {
	// Remove the transfer from the list of incoming transfers.
	transfer.Incoming().Remove(trnsfr)

	status := "success"
	if !successful {
		status = "failure"

		// First, drop the half-transferred server and its files so a failed
		// transfer cannot leak disk space on this node...
		manager.Remove(func(match *server.Server) bool {
			return match.ID() == trnsfr.Server.ID()
		})

		_ = trnsfr.Server.Filesystem().UnixFS().Close()
		if err := os.RemoveAll(trnsfr.Server.Filesystem().Path()); err != nil && !os.IsNotExist(err) {
			trnsfr.Log().WithError(err).Warn("failed to delete local server files for failed transfer")
		}
	}

	// Notify the panel after cleanup; a failed status call must not strand the
	// files or leave the server marked as transferring...
	if err := manager.Client().SetTransferStatus(context.Background(), trnsfr.Server.ID(), successful); err != nil {
		trnsfr.Log().WithField("status", status).WithError(err).Error("failed to set transfer status on panel")
	}

	trnsfr.Server.SetTransferring(false)
	trnsfr.Server.Events().Publish(server.TransferStatusEvent, status)
}
```

In `postTransfers` the deferred func becomes `defer func() { finalizeIncomingTransfer(manager, trnsfr, successful) }()` (keep reading `successful` at defer-run time).

- [ ] **Step 4: Run router tests, then full suite**; both green.
- [ ] **Step 5: Register in SGH-PATCHES.md and commit** `fix(transfers): always clean up failed incoming transfers` (include `docs/superpowers/plans/2026-09-01-upstream-issue-fixes.md` in this first commit).

---

### Task 2: Symlink-preserving extraction and restore (panel#5429)

**Files:**
- Modify: `server/filesystem/compress.go:277-304` (symlink branch in `extractStream`)
- Modify: `server/filesystem/filesystem.go:219` (`Symlink`: create parent, then link, then lchown)
- Modify: `server/backup/backup.go:39` (`RestoreCallback` takes `archives.FileInfo`)
- Modify: `server/backup/backup_local.go:115`, `server/backup/backup_s3.go` (pass `f` through)
- Modify: `server/backup.go:160` (`restoreBackupEntry` symlink branch)
- Test: extend `server/filesystem/compress_test.go`, `server/backup_restore_test.go`
- Modify: `SGH-PATCHES.md`

**Interfaces:**
- Consumes: `archives.FileInfo.LinkTarget`, `ufs.UnixFS.Symlink`, `Filesystem.chownFile` (already lchown + isTest bypass).
- Produces: `RestoreCallback func(file string, info archives.FileInfo, r io.ReadCloser) error`; `Filesystem.Symlink(oldpath, newpath string) error` that creates parent directories and chowns the link without following it.

- [ ] **Step 1: Write the failing round-trip test** in `server/filesystem/compress_test.go` (adapt helper names to that file's existing fixtures):

```go
// TestExtractStreamPreservesSymlinks archives a tree containing relative,
// absolute, and dangling symlinks with the production archiver and verifies
// extraction recreates real symlinks instead of empty regular files.
func TestExtractStreamPreservesSymlinks(t *testing.T) {
	sourceFs, _ := mkfs(t) // reuse this file's existing filesystem fixture helper
	targetFs, _ := mkfs(t)

	writeFile := func(fsys *Filesystem, name, contents string) {
		t.Helper()
		if err := fsys.Write(name, strings.NewReader(contents), int64(len(contents)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(sourceFs, "libraries/forge/unix_args.txt", "-jar forge.jar")
	if err := os.Symlink("libraries/forge/unix_args.txt", filepath.Join(sourceFs.Path(), "unix_args.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(sourceFs.Path(), "absolute_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing_target.txt", filepath.Join(sourceFs.Path(), "dangling_link")); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "transfer.tar.gz")
	if err := (&Archive{Filesystem: sourceFs}).Create(context.Background(), archivePath); err != nil {
		t.Fatal(err)
	}
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveFile.Close()

	if err := targetFs.ExtractStreamUnsafe(context.Background(), "/", archiveFile); err != nil {
		t.Fatal(err)
	}

	expectSymlink := func(name, wantTarget string) {
		t.Helper()
		info, err := os.Lstat(filepath.Join(targetFs.Path(), name))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to be a symlink, got mode %v", name, info.Mode())
		}
		target, err := os.Readlink(filepath.Join(targetFs.Path(), name))
		if err != nil {
			t.Fatal(err)
		}
		if target != wantTarget {
			t.Fatalf("expected %s to point at %q, got %q", name, wantTarget, target)
		}
	}
	expectSymlink("unix_args.txt", "libraries/forge/unix_args.txt")
	expectSymlink("absolute_link", "/etc/hostname")
	expectSymlink("dangling_link", "missing_target.txt")

	contents, err := os.ReadFile(filepath.Join(targetFs.Path(), "libraries/forge/unix_args.txt"))
	if err != nil || string(contents) != "-jar forge.jar" {
		t.Fatalf("expected regular file to survive the round trip, err %v contents %q", err, contents)
	}
}
```

- [ ] **Step 2: Run to verify failure**: symlink entries currently extract as empty regular files, so `expectSymlink` fails on the mode check.

- [ ] **Step 3: Implement extraction**:
  in `extractStream`'s callback (after the `f.IsDir()` branch, before `f.Open()`):

```go
		// Recreate symlinks as symlinks; writing them through Write would
		// materialize empty regular files at the link path...
		if f.Mode()&iofs.ModeSymlink != 0 {
			if err := fs.Symlink(f.LinkTarget, p); err != nil {
				return wrapError(err, opts.FileName)
			}
			return nil
		}
```

  and harden `Filesystem.Symlink` in `filesystem.go`:

```go
// Symlink creates newpath as a symbolic link to oldpath, creating any missing
// parent directories and giving the link itself the configured server
// ownership. The link target is deliberately not validated or chowned: links
// may point anywhere, and path resolution refuses to traverse links that
// escape the server root when they are later accessed.
func (fs *Filesystem) Symlink(oldpath, newpath string) error {
	// The link may arrive from an archive before its parent directory does...
	if err := fs.mkdirAll(filepath.Dir(newpath), 0o755); err != nil {
		return err
	}
	if err := fs.unixFS.Symlink(oldpath, newpath); err != nil {
		return err
	}
	return fs.chownFile(newpath)
}
```

  (`chownFile` already uses `Lchown`, so the chown applies to the link itself, never the target.)

- [ ] **Step 4: Run filesystem tests**; round trip green.

- [ ] **Step 5: Write the failing restore test** by extending `TestBackupRestoreRoundTrip` in `server/backup_restore_test.go`: add to the source tree `require.NoError(t, os.Symlink("world/region/r.0.0.mca", filepath.Join(sourceFilesystem.Path(), "region_link")))` after the region file is written, and after restore assert with `os.Lstat` + `os.Readlink` that `region_link` is a symlink pointing at `world/region/r.0.0.mca`.

- [ ] **Step 6: Run to verify failure**, then **implement restore**: change `RestoreCallback` in `server/backup/backup.go` to `func(file string, info archives.FileInfo, r io.ReadCloser) error` (import `github.com/mholt/archives`, drop `io/fs` if unused), pass `f` instead of `f.FileInfo` in `backup_local.go` and `backup_s3.go`, and in `server/backup.go` change `restoreBackupEntry` to take `info archives.FileInfo` and add after the directory branch:

```go
	// Symlink entries carry their target in the archive metadata and must be
	// recreated as links rather than written out as empty files...
	if info.Mode()&fs.ModeSymlink != 0 {
		return s.Filesystem().Symlink(info.LinkTarget, file)
	}
```

  (skip `Chtimes` for the symlink branch; `Chtimes` follows links.)

- [ ] **Step 7: Run server + backup + filesystem packages, then full suite**; green.
- [ ] **Step 8: Register in SGH-PATCHES.md and commit** `fix(files): preserve symlinks during extraction and restore`.

---

### Task 3: Honest power actions during boot (panel#5712)

**Files:**
- Modify: `router/router_server.go:53-105` (409 pre-check in `postServerPower`)
- Modify: `environment/docker/environment.go:42` (`client client.APIClient` interface type)
- Modify: `environment/docker/power.go:280-325` (`SignalContainer` starting-state guard, `Terminate` conditional offline)
- Create: `router/router_server_power_test.go`, `environment/docker/power_test.go`
- Modify: `SGH-PATCHES.md`

**Interfaces:**
- Consumes: `server.Server.ExecutingPowerAction()`, `environment.ProcessEnvironment` (fake in router test), `client.APIClient` (fake in docker test).
- Produces: HTTP 409 `{"error": "..."}` from `POST /power` for start/stop/restart with `wait_seconds<=0` while a power action is executing; `SignalContainer`/`Terminate` that never force a `starting` environment offline without having stopped anything.

- [ ] **Step 1: Write the failing router test** in `router/router_server_power_test.go`:

```go
package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
)

// blockingTestEnvironment satisfies environment.ProcessEnvironment with
// no-ops, except WaitForStop, which blocks until release is closed so a test
// can hold the server's power lock the same way a real boot does.
type blockingTestEnvironment struct {
	environment.ProcessEnvironment

	release chan struct{}
}

func (e *blockingTestEnvironment) WaitForStop(ctx context.Context, _ time.Duration, _ bool) error {
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil
}

// If embedding the interface panics on other calls at runtime, implement the
// remaining methods used by the stop path explicitly with no-ops.

// newPowerFixture returns a server whose power lock is held by a fake
// in-flight stop action, plus a request context for the power endpoint.
func newPowerFixture(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder, *wserver.Server, chan struct{}) {
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
	s.Environment = &blockingTestEnvironment{release: release}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

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
	return c, recorder, s, release
}

func TestPostServerPowerConflictsWhileAnotherActionRuns(t *testing.T) {
	c, recorder, _, _ := newPowerFixture(t, `{"action":"stop"}`)

	postServerPower(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected stop during an executing power action to return 409, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestPostServerPowerStillAcceptsKillWhileLocked(t *testing.T) {
	c, recorder, _, _ := newPowerFixture(t, `{"action":"kill"}`)

	postServerPower(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected kill to bypass the conflict check, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestPostServerPowerStillQueuesExplicitWaits(t *testing.T) {
	c, recorder, _, release := newPowerFixture(t, `{"action":"stop","wait_seconds":10}`)
	close(release)

	postServerPower(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected an explicit wait_seconds request to be accepted, got %d body %s", recorder.Code, recorder.Body.String())
	}
}
```

- [ ] **Step 2: Run to verify failure** (409 test gets 202 today).

- [ ] **Step 3: Implement the router check** in `postServerPower` after the suspension check:

```go
	// A request that would not wait for the lock is dropped silently by the
	// background goroutine today, so reject it here where the caller can see
	// it. Termination stays exempt as the escape hatch for stuck servers, and
	// callers that pass wait_seconds keep their queueing behavior...
	if data.Action != server.PowerActionTerminate && data.WaitSeconds <= 0 && s.ExecutingPowerAction() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "A power action is already being processed for this server, please try again later.",
		})
		return
	}
```

- [ ] **Step 4: Run router tests**; green.

- [ ] **Step 5: Write the failing docker environment test** in `environment/docker/power_test.go` after switching the `client` field to the `client.APIClient` interface (constructor stays `New(...)`, assignment unchanged since `*client.Client` satisfies the interface). Build a fake:

```go
package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/system"
)

// fakeDockerClient satisfies client.APIClient through embedding and overrides
// only the calls the termination path makes.
type fakeDockerClient struct {
	client.APIClient

	inspect       container.InspectResponse
	inspectErr    error
	killedSignal  string
	removedForced bool
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	return f.inspect, f.inspectErr
}

func (f *fakeDockerClient) ContainerKill(_ context.Context, _ string, signal string) error {
	f.killedSignal = signal
	return nil
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, _ string, opts container.RemoveOptions) error {
	f.removedForced = opts.Force
	return nil
}

// newTestEnvironment builds an Environment around the fake client with the
// minimum wiring SetState needs (events bus and state atomics).
func newTestEnvironment(fake *fakeDockerClient, state string) *Environment {
	e := &Environment{
		Id:     "test-environment",
		client: fake,
		meta:   &Metadata{Stop: remote.ProcessStopConfiguration{}},
	}
	e.events = events.NewBus()
	e.st = system.NewAtomic(state)
	return e
}
```

(Adjust construction to the real `Environment` field set: mirror `New()` in `environment.go`, calling it if exported construction is simpler than struct literals, and use whatever inspect-response types the SDK version in go.mod exposes.)

Tests:

```go
func TestTerminateDoesNotMarkABootingServerOffline(t *testing.T) {
	fake := &fakeDockerClient{inspect: notRunningInspect("created")}
	e := newTestEnvironment(fake, environment.ProcessStartingState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessStartingState {
		t.Fatalf("expected environment to stay in starting state, got %q", got)
	}
	if !fake.removedForced {
		t.Fatal("expected the created container to be force removed so the boot aborts")
	}
}

func TestTerminateStillStopsARunningServer(t *testing.T) {
	fake := &fakeDockerClient{inspect: runningInspect()}
	e := newTestEnvironment(fake, environment.ProcessRunningState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessOfflineState {
		t.Fatalf("expected a running server to be marked offline after terminate, got %q", got)
	}
	if fake.killedSignal != "SIGKILL" {
		t.Fatalf("expected SIGKILL to be sent, got %q", fake.killedSignal)
	}
}

func TestTerminateBeforeContainerCreationReportsInstead(t *testing.T) {
	fake := &fakeDockerClient{inspectErr: errNotFound{}} // helper satisfying client.IsErrNotFound
	e := newTestEnvironment(fake, environment.ProcessStartingState)

	err := e.Terminate(context.Background(), "SIGKILL")
	if err == nil {
		t.Fatal("expected terminating before the container exists to report an error instead of faking offline")
	}
	if got := e.State(); got != environment.ProcessStartingState {
		t.Fatalf("expected environment to stay in starting state, got %q", got)
	}
}
```

- [ ] **Step 6: Run to verify failure**, then **implement the guards** in `environment/docker/power.go`:

```go
// Sends the specified signal to the container in an attempt to stop it.
func (e *Environment) SignalContainer(ctx context.Context, signal string) error {
	c, err := e.ContainerInspect(ctx)
	if err != nil {
		if !client.IsErrNotFound(err) {
			return errors.WithStack(err)
		}

		// A booting server has not created its container yet; pretending the
		// signal worked would mark a starting server offline while the boot
		// keeps running, so report the situation honestly instead...
		if e.st.Load() == environment.ProcessStartingState {
			return errors.New("environment/docker: cannot signal server: container not yet created, try again shortly")
		}
		return nil
	}

	if !c.State.Running {
		// A created-but-not-started container mid-boot is aborted by force
		// removing it; the boot goroutine's own error handling then walks the
		// state to offline without tripping crash detection...
		if e.st.Load() == environment.ProcessStartingState {
			return errors.WithStack(e.client.ContainerRemove(ctx, e.Id, container.RemoveOptions{Force: true}))
		}

		// If the container is not running, but we're not already in a stopped state go ahead
		// and update things to indicate we should be completely stopped now. Set to stopping
		// first so crash detection is not triggered.
		if e.st.Load() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}

	// We set it to stopping than offline to prevent crash detection from being triggered.
	e.SetState(environment.ProcessStoppingState)
	if err := e.client.ContainerKill(ctx, e.Id, signal); err != nil && !client.IsErrNotFound(err) {
		return errors.WithStack(err)
	}
	return nil
}

// Terminate forcefully terminates the container using the signal provided.
// then sets its state to stopped, unless the server is still booting, in
// which case the boot's own failure handling owns the state transition.
func (e *Environment) Terminate(ctx context.Context, signal string) error {
	if err := e.SignalContainer(ctx, signal); err != nil {
		return errors.WithStack(err)
	}

	if e.st.Load() == environment.ProcessStartingState {
		return nil
	}

	e.SetState(environment.ProcessOfflineState)
	return nil
}
```

Note: the running-container branch sets state to stopping before Terminate re-checks, so use the state captured BEFORE `SignalContainer` for the Terminate guard if tests show the stopping transition masks it (capture `wasStarting := e.st.Load() == environment.ProcessStartingState` up front).

- [ ] **Step 7: Run docker + router + server packages, then full suite**; green.
- [ ] **Step 8: Register in SGH-PATCHES.md and commit** `fix(power): surface and contain power actions during boot`.

---

### Task 4: Asynchronous decompression (panel#2878)

**Files:**
- Modify: `router/router_server_files.go:452-494` (`postServerDecompressFiles`)
- Create: `router/router_server_files_decompress_test.go`
- Modify: `SGH-PATCHES.md`

**Interfaces:**
- Consumes: `Filesystem.SpaceAvailableForDecompression`, `Filesystem.DecompressFile`, `Server.PublishConsoleOutputFromDaemon`, `Server.Context()`.
- Produces: `POST /files/decompress` returns 202 immediately after synchronous existence and format validation; progress and errors surface as daemon console lines.

- [ ] **Step 1: Write the failing tests** in `router/router_server_files_decompress_test.go` (fixture mirrors `newFingerprintContext`; a helper builds a real tar.gz inside the server root using `archive/tar` + `compress/gzip` with one entry `unpacked/hello.txt` containing `hello world`):

```go
func TestPostServerDecompressFilesReturnsAcceptedAndExtractsInBackground(t *testing.T) {
	c, recorder, s := newDecompressContext(t, `{"root":"/","file":"bundle.tar.gz"}`)
	writeTestTarball(t, s, "bundle.tar.gz") // helper: real tar.gz written through s.Filesystem()

	postServerDecompressFiles(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected async decompression to return 202, got %d body %s", recorder.Code, recorder.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	extracted := filepath.Join(s.Filesystem().Path(), "unpacked/hello.txt")
	for {
		if contents, err := os.ReadFile(extracted); err == nil {
			if string(contents) != "hello world" {
				t.Fatalf("expected extracted contents to match, got %q", contents)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the background extraction to finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPostServerDecompressFilesRejectsUnknownFormatSynchronously(t *testing.T) {
	c, recorder, s := newDecompressContext(t, `{"root":"/","file":"noise.bin"}`)
	if err := s.Filesystem().Write("noise.bin", strings.NewReader("not an archive"), 14, 0o644); err != nil {
		t.Fatal(err)
	}

	postServerDecompressFiles(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown archive format to return 400, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestPostServerDecompressFilesRejectsMissingFileSynchronously(t *testing.T) {
	c, recorder, _ := newDecompressContext(t, `{"root":"/","file":"missing.tar.gz"}`)

	postServerDecompressFiles(c)

	if recorder.Code >= 500 || recorder.Code == http.StatusAccepted {
		t.Fatalf("expected a missing archive to fail fast with a client error, got %d", recorder.Code)
	}
}
```

(For the missing-file case the handler aborts through `middleware.CaptureAndAbort`; in a bare test context assert only that it did not 202 and did not 5xx-panic, the middleware mapping to 404 is covered by existing middleware tests.)

- [ ] **Step 2: Run to verify failure** (202 test gets 204 today after blocking).

- [ ] **Step 3: Implement**:

```go
// postServerDecompressFiles validates the archive quickly and then unpacks it
// in the background: multi-gigabyte archives routinely outlive proxy timeouts,
// which made users retry and double-extract, so the request must not block on
// the extraction itself.
func postServerDecompressFiles(c *gin.Context) {
	var data struct {
		RootPath string `json:"root"`
		File     string `json:"file"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	s := middleware.ExtractServer(c)
	lg := middleware.ExtractLogger(c).WithFields(log.Fields{"root_path": data.RootPath, "file": data.File})

	// Reject requests for archives that are missing or unreadable up front so
	// the caller still gets an immediate, accurate error...
	sourcePath := filepath.Join(data.RootPath, data.File)
	if _, err := s.Filesystem().Stat(sourcePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Sniffing the format only reads the archive header, so keep that check
	// synchronous as well; the expensive full-archive walk happens later...
	if err := s.Filesystem().CanDecompressFile(c.Request.Context(), data.RootPath, data.File); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeUnknownArchive) {
			lg.WithField("error", err).Warn("failed to decompress file: unknown archive format")
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The archive provided is in a format Wings does not understand."})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	go func(s *server.Server, rootPath, file string) {
		s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Decompressing archive %s, this may take a while...", file))

		if err := s.Filesystem().SpaceAvailableForDecompression(s.Context(), rootPath, file); err != nil {
			s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Cannot decompress %s: not enough disk space is available.", file))
			s.Log().WithFields(log.Fields{"root_path": rootPath, "file": file, "error": err}).Warn("failed to decompress file: insufficient space")
			return
		}

		if err := s.Filesystem().DecompressFile(s.Context(), rootPath, file); err != nil {
			if strings.Contains(err.Error(), "text file busy") {
				s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Cannot decompress %s: one or more files are in use by the running server process.", file))
			} else {
				s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Decompressing %s failed; check the node logs for details.", file))
			}
			s.Log().WithFields(log.Fields{"root_path": rootPath, "file": file, "error": err}).Error("failed to decompress file in background")
			return
		}

		s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Finished decompressing %s.", file))
	}(s, data.RootPath, data.File)

	c.Status(http.StatusAccepted)
}
```

Add the small `CanDecompressFile` helper in `server/filesystem/compress.go` (open + `archives.Identify` + close, returning `ErrCodeUnknownArchive` exactly like `DecompressFile` does, without extracting).
If `Filesystem.Stat` does not exist under that name, use the fork's actual stat helper (`s.Filesystem().File(...)`/`Lstat` equivalent) and close anything opened.

- [ ] **Step 4: Run router tests, then full suite**; green.
- [ ] **Step 5: Register in SGH-PATCHES.md and commit** `feat(files): decompress archives in the background`.

---

### Final verification (after Task 4)

- [ ] `gofmt -l .` empty, `go vet ./...` clean.
- [ ] Full suite plus `CGO_ENABLED=1 go test -race ./...` in the container; all green.
- [ ] Re-read SGH-PATCHES.md entries for the four patches: files lists accurate, one sentence per line.
- [ ] Report: four commits on `codex/upstream-issue-fixes`, per-fix summary, what was NOT covered (no live two-node transfer, no real docker daemon boot test, zip-symlink path untested) and suggested canary checks.
