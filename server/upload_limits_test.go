package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server/filesystem"
)

// TestMain installs normal configuration defaults before isolated server package tests run.
func TestMain(m *testing.M) {
	configuration, err := config.NewAtPath("")
	if err != nil {
		panic(err)
	}
	configuration.AuthenticationToken = "server-test-secret"
	config.Set(configuration)
	os.Exit(m.Run())
}

// TestUploadRequestGuardsEnforceRateScopes verifies one abusive identity cannot exceed its window.
func TestUploadRequestGuardsEnforceRateScopes(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	manager.limits.CreationRatePerMinuteUser = 1
	manager.limits.CreationRatePerMinuteServer = 10
	manager.limits.CreationRatePerMinuteNode = 10

	if err := manager.AllowCreation("server-one", "user-one"); err != nil {
		t.Fatalf("expected first creation to pass: %v", err)
	}
	err := manager.AllowCreation("server-one", "user-one")
	if !errors.Is(err, ErrUploadRateLimited) {
		t.Fatalf("expected second user request to be rate limited, got %v", err)
	}
	var rateLimit *UploadRateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.RetryAfter <= 0 {
		t.Fatalf("expected an actionable retry duration, got %v", err)
	}
	if err := manager.AllowCreation("server-one", "user-two"); err != nil {
		t.Fatalf("expected a different user to retain capacity: %v", err)
	}
}

// TestUploadRequestGuardsReleaseConcurrency verifies completed bodies immediately restore capacity.
func TestUploadRequestGuardsReleaseConcurrency(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	manager.limits.MaxConcurrentPerUser = 1
	manager.limits.MaxConcurrentPerServer = 2
	manager.limits.MaxConcurrentNode = 2

	release, err := manager.AcquireTransfer("server-one", "user-one")
	if err != nil {
		t.Fatalf("expected first transfer to acquire capacity: %v", err)
	}
	if _, err := manager.AcquireTransfer("server-one", "user-one"); !errors.Is(err, ErrUploadBusy) {
		t.Fatalf("expected concurrent user transfer to be rejected, got %v", err)
	}
	release()
	release()

	secondRelease, err := manager.AcquireTransfer("server-one", "user-one")
	if err != nil {
		t.Fatalf("expected released capacity to be reusable: %v", err)
	}
	secondRelease()
}

// TestUploadStartupIndexRollsBackAfterCleanupFailure prevents failed boots from leaking cap counters.
func TestUploadStartupIndexRollsBackAfterCleanupFailure(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	manager.limits.MaxActiveSessionsPerUser = 1
	serverUUID := uuid.NewString()
	userUUID := uuid.NewString()
	resolved := &Server{}
	resolved.cfg.Uuid = serverUUID
	now := time.Now().UTC()
	sessions := []loadedUploadSession{
		{session: UploadSession{ID: uuid.NewString(), ServerUUID: serverUUID, UserUUID: userUUID, Target: "first.tar", State: uploadSessionActive, UpdatedAt: now}, size: 100},
		{session: UploadSession{ID: uuid.NewString(), ServerUUID: serverUUID, UserUUID: userUUID, Target: "second.tar", State: uploadSessionActive, UpdatedAt: now}, size: 100},
	}
	cleanupError := errors.New("injected cleanup failure")
	err := manager.indexServerSessions(resolved, sessions, func(*Server, UploadSession) error {
		return cleanupError
	})
	if !errors.Is(err, cleanupError) {
		t.Fatalf("expected cleanup failure, got %v", err)
	}

	manager.indexMu.RLock()
	defer manager.indexMu.RUnlock()
	if manager.totalStored != 0 || manager.activeNode != 0 || len(manager.sessions[serverUUID]) != 0 {
		t.Fatalf("expected startup indexing rollback, stored=%d active=%d sessions=%d", manager.totalStored, manager.activeNode, len(manager.sessions[serverUUID]))
	}
}

// TestUploadStartupRetentionKeepsNewestSession makes restart eviction independent of directory order.
func TestUploadStartupRetentionKeepsNewestSession(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	serverUUID := uuid.NewString()
	resolved := &Server{}
	resolved.cfg.Uuid = serverUUID
	now := time.Now().UTC()
	oldest := UploadSession{ID: uuid.NewString(), ServerUUID: serverUUID, State: uploadSessionComplete, UpdatedAt: now.Add(-time.Minute)}
	newest := UploadSession{ID: uuid.NewString(), ServerUUID: serverUUID, State: uploadSessionComplete, UpdatedAt: now}

	candidates := loadedUploadSessionHeap{}
	var candidateBytes int64
	for _, session := range []UploadSession{oldest, newest} {
		data, err := json.Marshal(session)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.persistSession(session, data); err != nil {
			t.Fatal(err)
		}
		candidate := loadedUploadSession{session: session, size: int64(len(data))}
		if err := manager.retainServerSessionCandidate(resolved, &candidates, &candidateBytes, candidate, 1, 1024*1024); err != nil {
			t.Fatal(err)
		}
	}

	if len(candidates) != 1 || candidates[0].session.ID != newest.ID {
		t.Fatalf("expected newest session %s to survive, got %+v", newest.ID, candidates)
	}
	if _, err := os.Stat(manager.sessionPath(serverUUID, oldest.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected oldest metadata to be evicted, got %v", err)
	}
	if _, err := os.Stat(manager.sessionPath(serverUUID, newest.ID)); err != nil {
		t.Fatalf("expected newest metadata to remain: %v", err)
	}
}

// TestStoredUploadSessionRejectsRootTraversal prevents tampered restart state from targeting the parent directory.
func TestStoredUploadSessionRejectsRootTraversal(t *testing.T) {
	now := time.Now().UTC()
	uploadID := "12289395-ea24-4d6d-b8cb-e2c4081a9f61"
	session := UploadSession{
		ID:          uploadID,
		ServerUUID:  "8d7cf8e6-9997-4809-bcc2-0eed6bfe8cbf",
		UserUUID:    "bdae9921-0fea-4a7a-935c-347e224b3e29",
		Target:      "..",
		Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:        1,
		State:       uploadSessionActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	session.Partial = expectedUploadPartial(session)

	if validStoredUploadSession(session.ServerUUID, uploadID+".json", session, NewUploadManager(t.TempDir()).limits) {
		t.Fatal("expected the parent-directory target to be rejected")
	}
}

// TestUploadPurgeWaitsForInFlightCreation prevents server deletion from racing durable session creation.
func TestUploadPurgeWaitsForInFlightCreation(t *testing.T) {
	root := t.TempDir()
	serverFilesystem, err := filesystem.New(filepath.Join(root, "volume"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverFilesystem.UnixFS().Close()

	serverUUID := uuid.NewString()
	resolved := &Server{fs: serverFilesystem}
	resolved.cfg.Uuid = serverUUID
	manager := NewUploadManager(filepath.Join(root, "metadata"))
	manager.loadedServers[serverUUID] = true

	// First, stop creation after it owns the lifecycle and destination locks...
	manager.createMu.Lock()
	creationLocked := true
	defer func() {
		if creationLocked {
			manager.createMu.Unlock()
		}
	}()
	userUUID := uuid.NewString()
	uploadID := uuid.NewString()
	creation := make(chan error, 1)
	go func() {
		_, err := manager.Create(
			resolved,
			userUUID,
			uploadID,
			"world.tar",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			1,
		)
		creation <- err
	}()
	waitForUploadTargetLock(t, manager, uploadTargetLockKey(serverUUID, "world.tar"))

	// Next, confirm purge cannot pass the in-flight creation lifecycle boundary...
	purge := make(chan error, 1)
	go func() { purge <- manager.PurgeServer(resolved) }()
	select {
	case err := <-purge:
		t.Fatalf("purge returned before in-flight creation finished: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	manager.createMu.Unlock()
	creationLocked = false
	if err := <-creation; err != nil {
		t.Fatalf("expected in-flight creation to finish before purge: %v", err)
	}
	if err := <-purge; err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.indexedSession(serverUUID, uploadID); ok {
		t.Fatal("expected purged server state to remain inaccessible")
	}
	if _, err := os.Stat(manager.serverDirectory(serverUUID)); !os.IsNotExist(err) {
		t.Fatalf("expected purged metadata directory to be removed, got %v", err)
	}
	manager.indexMu.RLock()
	defer manager.indexMu.RUnlock()
	if manager.totalStored != 0 || manager.activeNode != 0 {
		t.Fatalf("expected purge to release session accounting, stored=%d active=%d", manager.totalStored, manager.activeNode)
	}
}

// waitForUploadTargetLock waits until a concurrent operation owns or awaits one destination lock.
func waitForUploadTargetLock(t *testing.T, manager *UploadManager, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		manager.locks.mu.Lock()
		_, exists := manager.locks.entries[key]
		manager.locks.mu.Unlock()
		if exists {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for upload target lock")
}
