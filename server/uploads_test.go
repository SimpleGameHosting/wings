package server

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/server/filesystem"
)

// newUploadTestServer builds a server backed by a real temporary volume and a
// manager that already considers it loaded, which is the state every request
// path expects once InitializeServer has run.
func newUploadTestServer(t *testing.T) (*Server, *UploadManager) {
	t.Helper()

	root := t.TempDir()
	serverFilesystem, err := filesystem.New(filepath.Join(root, "volume"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverFilesystem.UnixFS().Close() })

	s := &Server{fs: serverFilesystem}
	s.cfg.Uuid = uuid.NewString()
	manager := NewUploadManager(filepath.Join(root, "metadata"))
	manager.loadedServers[s.ID()] = true
	return s, manager
}

// newUploadTestSession describes an active upload of content to target whose
// fingerprint and partial path are the ones the manager would derive itself.
func newUploadTestSession(s *Server, target string, content []byte) UploadSession {
	now := time.Now().UTC()
	sum := sha256.Sum256(content)
	session := UploadSession{
		ID:          uuid.NewString(),
		ServerUUID:  s.ID(),
		UserUUID:    uuid.NewString(),
		Target:      target,
		Fingerprint: hex.EncodeToString(sum[:]),
		Size:        int64(len(content)),
		State:       uploadSessionActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	session.Partial = expectedUploadPartial(session)
	return session
}

// persistUploadTestSession writes a session's metadata file the way the manager does.
func persistUploadTestSession(t *testing.T, manager *UploadManager, session UploadSession) int64 {
	t.Helper()
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.persistSession(session, data); err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

// readUploadTestSession decodes the metadata file the manager holds for a session.
func readUploadTestSession(t *testing.T, manager *UploadManager, session UploadSession) UploadSession {
	t.Helper()
	data, err := os.ReadFile(manager.sessionPath(session.ServerUUID, session.ID))
	if err != nil {
		t.Fatal(err)
	}
	var stored UploadSession
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestBoundedUploadWriterAcceptsBytesWithinTheDeclaredLength passes a chunk
// that fits straight through and keeps counting down the remaining budget.
func TestBoundedUploadWriterAcceptsBytesWithinTheDeclaredLength(t *testing.T) {
	var sink bytes.Buffer
	writer := &boundedUploadWriter{writer: &sink, remaining: 10}

	written, err := writer.Write([]byte("abcd"))

	if err != nil || written != 4 {
		t.Fatalf("expected a clean 4 byte write, got written=%d err=%v", written, err)
	}
	if writer.remaining != 6 || sink.String() != "abcd" {
		t.Fatalf("expected 6 bytes remaining and the chunk stored, got remaining=%d stored=%q", writer.remaining, sink.String())
	}
}

// TestBoundedUploadWriterTruncatesAnOverflowingChunk stores only the bytes
// that fit and reports the overflow, so a declared length is never exceeded.
func TestBoundedUploadWriterTruncatesAnOverflowingChunk(t *testing.T) {
	var sink bytes.Buffer
	writer := &boundedUploadWriter{writer: &sink, remaining: 3}

	written, err := writer.Write([]byte("hello"))

	if !errors.Is(err, ErrUploadTooLarge) || written != 3 {
		t.Fatalf("expected 3 bytes and ErrUploadTooLarge, got written=%d err=%v", written, err)
	}
	if writer.remaining != 0 || sink.String() != "hel" {
		t.Fatalf("expected the budget exhausted with the prefix stored, got remaining=%d stored=%q", writer.remaining, sink.String())
	}
}

// TestBoundedUploadWriterRejectsBytesOnceFull refuses any further bytes once
// the declared length has been reached, without touching the sink.
func TestBoundedUploadWriterRejectsBytesOnceFull(t *testing.T) {
	var sink bytes.Buffer
	writer := &boundedUploadWriter{writer: &sink, remaining: 0}

	written, err := writer.Write([]byte("x"))

	if !errors.Is(err, ErrUploadTooLarge) || written != 0 || sink.Len() != 0 {
		t.Fatalf("expected nothing written and ErrUploadTooLarge, got written=%d stored=%d err=%v", written, sink.Len(), err)
	}
}

// TestUploadContextReaderStopsOnceTheRequestIsCancelled delegates reads while
// the request is live and fails with the context error as soon as it is not.
func TestUploadContextReaderStopsOnceTheRequestIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &uploadContextReader{ctx: ctx, reader: strings.NewReader("payload")}
	buffer := make([]byte, 3)

	read, err := reader.Read(buffer)
	if err != nil || read != 3 || string(buffer) != "pay" {
		t.Fatalf("expected a delegated read, got read=%d buffer=%q err=%v", read, buffer, err)
	}

	cancel()
	read, err = reader.Read(buffer)
	if !errors.Is(err, context.Canceled) || read != 0 {
		t.Fatalf("expected the cancellation to surface, got read=%d err=%v", read, err)
	}
}

// TestUploadTargetLocksSerializeHoldersAndDropIdleEntries proves a second
// caller waits for the first holder and that a released key leaves no entry
// behind, since the map would otherwise grow with every upload ever seen.
func TestUploadTargetLocksSerializeHoldersAndDropIdleEntries(t *testing.T) {
	locks := uploadTargetLocks{entries: make(map[string]*uploadTargetLock)}
	unlock := locks.lock("server:world.tar")
	acquired := make(chan struct{})
	go func() {
		release := locks.lock("server:world.tar")
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("the second holder acquired the lock while the first still held it")
	case <-time.After(25 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the second holder never acquired the released lock")
	}

	deadline := time.Now().Add(time.Second)
	for {
		locks.mu.Lock()
		remaining := len(locks.entries)
		locks.mu.Unlock()
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected the idle lock entry to be dropped, %d entries remain", remaining)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestUploadSessionRetentionDependsOnState keeps an unfinished upload for a
// day and a published one only long enough for the client to read its status.
func TestUploadSessionRetentionDependsOnState(t *testing.T) {
	updated := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	active := UploadSession{State: uploadSessionActive, UpdatedAt: updated}
	complete := UploadSession{State: uploadSessionComplete, UpdatedAt: updated}

	if !active.ExpiresAt().Equal(updated.Add(24 * time.Hour)) {
		t.Fatalf("expected an active session to live for a day, got %s", active.ExpiresAt())
	}
	if !complete.ExpiresAt().Equal(updated.Add(15 * time.Minute)) {
		t.Fatalf("expected a complete session to live for fifteen minutes, got %s", complete.ExpiresAt())
	}
	if active.Expired(active.ExpiresAt().Add(-time.Nanosecond)) {
		t.Fatal("a session must not expire before its deadline")
	}
	if !active.Expired(active.ExpiresAt()) {
		t.Fatal("a session must expire exactly at its deadline")
	}
}

// TestStoredUploadSessionRejectsTamperedMetadata walks every rule that keeps a
// hand-edited metadata file from steering the manager at an arbitrary path or
// bypassing its identity checks, and confirms the untouched baseline passes.
func TestStoredUploadSessionRejectsTamperedMetadata(t *testing.T) {
	s := &Server{}
	s.cfg.Uuid = uuid.NewString()
	limits := NewUploadManager(t.TempDir()).limits
	limits.MaxPathBytes = 64
	limits.MaxFilenameBytes = 32
	baseline := newUploadTestSession(s, "world/level.dat", []byte("level"))

	if !validStoredUploadSession(s.ID(), baseline.ID+".json", baseline, limits) {
		t.Fatal("the baseline session must be accepted")
	}

	cases := []struct {
		name     string
		filename string
		mutate   func(*UploadSession)
		// keepPartial leaves the partial path as the mutation set it, instead
		// of re-deriving it so the rule under test is the only reason to reject.
		keepPartial bool
	}{
		{name: "filename does not match the id", filename: uuid.NewString() + ".json"},
		{name: "foreign server", mutate: func(u *UploadSession) { u.ServerUUID = uuid.NewString() }},
		{name: "negative size", mutate: func(u *UploadSession) { u.Size = -1 }},
		{name: "non-uuid id", filename: "not-a-uuid.json", mutate: func(u *UploadSession) { u.ID = "not-a-uuid" }},
		{name: "non-uuid user", mutate: func(u *UploadSession) { u.UserUUID = "nobody" }},
		{name: "zero created time", mutate: func(u *UploadSession) { u.CreatedAt = time.Time{} }},
		{name: "updated before created", mutate: func(u *UploadSession) { u.UpdatedAt = u.CreatedAt.Add(-time.Second) }},
		{name: "unknown state", mutate: func(u *UploadSession) { u.State = "paused" }},
		{name: "absolute target", mutate: func(u *UploadSession) { u.Target = "/etc/passwd" }},
		{name: "current directory target", mutate: func(u *UploadSession) { u.Target = "." }},
		{name: "parent traversal", mutate: func(u *UploadSession) { u.Target = "../level.dat" }},
		{name: "uncleaned target", mutate: func(u *UploadSession) { u.Target = "world//level.dat" }},
		{name: "target over the path cap", mutate: func(u *UploadSession) { u.Target = strings.Repeat("a", 65) }},
		{name: "segment over the filename cap", mutate: func(u *UploadSession) { u.Target = "world/" + strings.Repeat("a", 33) }},
		{name: "partial path mismatch", keepPartial: true, mutate: func(u *UploadSession) { u.Partial = "elsewhere.part" }},
		{name: "short fingerprint", mutate: func(u *UploadSession) { u.Fingerprint = "abcd" }},
		{name: "non-hex fingerprint", mutate: func(u *UploadSession) { u.Fingerprint = strings.Repeat("z", 64) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := baseline
			if tc.mutate != nil {
				tc.mutate(&session)
			}
			if !tc.keepPartial {
				session.Partial = expectedUploadPartial(session)
			}
			filename := tc.filename
			if filename == "" {
				filename = baseline.ID + ".json"
			}
			if validStoredUploadSession(s.ID(), filename, session, limits) {
				t.Fatalf("expected the session to be rejected: %+v", session)
			}
		})
	}
}

// TestLoadedUploadSessionHeapPopsTheOldestFirstWithStableTies is what makes
// restart eviction independent of directory listing order.
func TestLoadedUploadSessionHeapPopsTheOldestFirstWithStableTies(t *testing.T) {
	now := time.Now().UTC()
	candidates := loadedUploadSessionHeap{}
	heap.Init(&candidates)
	for _, candidate := range []loadedUploadSession{
		{session: UploadSession{ID: "b", UpdatedAt: now}},
		{session: UploadSession{ID: "a", UpdatedAt: now}},
		{session: UploadSession{ID: "c", UpdatedAt: now.Add(-time.Minute)}},
		{session: UploadSession{ID: "d", UpdatedAt: now.Add(time.Minute)}},
	} {
		heap.Push(&candidates, candidate)
	}

	var order []string
	for candidates.Len() > 0 {
		order = append(order, heap.Pop(&candidates).(loadedUploadSession).session.ID)
	}

	if strings.Join(order, "") != "cabd" {
		t.Fatalf("expected oldest first with ties broken by id, got %v", order)
	}
}

// TestRetainServerSessionCandidateEvictsByMetadataBytes keeps the newest
// sessions once the byte budget for restart metadata is exceeded and deletes
// the evicted metadata from disk.
func TestRetainServerSessionCandidateEvictsByMetadataBytes(t *testing.T) {
	s, manager := newUploadTestServer(t)
	now := time.Now().UTC()
	var sessions []loadedUploadSession
	var total int64
	for offset := 3; offset >= 1; offset-- {
		session := newUploadTestSession(s, "world.tar", []byte("data"))
		session.State = uploadSessionComplete
		session.UpdatedAt = now.Add(-time.Duration(offset) * time.Minute)
		size := persistUploadTestSession(t, manager, session)
		sessions = append(sessions, loadedUploadSession{session: session, size: size})
		total += size
	}
	oldest := sessions[0]

	candidates := loadedUploadSessionHeap{}
	var candidateBytes int64
	for _, candidate := range sessions {
		if err := manager.retainServerSessionCandidate(s, &candidates, &candidateBytes, candidate, 10, total-1); err != nil {
			t.Fatal(err)
		}
	}

	if len(candidates) != 2 || candidateBytes != total-oldest.size {
		t.Fatalf("expected the oldest candidate evicted, got %d candidates holding %d bytes", len(candidates), candidateBytes)
	}
	for _, candidate := range candidates {
		if candidate.session.ID == oldest.session.ID {
			t.Fatal("the oldest session survived eviction")
		}
	}
	if _, err := os.Stat(manager.sessionPath(s.ID(), oldest.session.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected the evicted metadata to be removed, got %v", err)
	}
}

// TestReadServerSessionsDropsUnusableMetadataAndKeepsTheRest removes files
// the manager cannot trust, oversized, unparsable, misnamed, or expired,
// while leaving the valid session and anything that is not metadata alone.
func TestReadServerSessionsDropsUnusableMetadataAndKeepsTheRest(t *testing.T) {
	s, manager := newUploadTestServer(t)
	directory := manager.serverDirectory(s.ID())
	valid := newUploadTestSession(s, "world.tar", []byte("data"))
	persistUploadTestSession(t, manager, valid)

	expired := newUploadTestSession(s, "old.tar", []byte("data"))
	expired.CreatedAt = expired.CreatedAt.Add(-26 * time.Hour)
	expired.UpdatedAt = expired.CreatedAt
	persistUploadTestSession(t, manager, expired)

	misnamed := newUploadTestSession(s, "moved.tar", []byte("data"))
	misnamedData, err := json.Marshal(misnamed)
	if err != nil {
		t.Fatal(err)
	}
	misnamedPath := filepath.Join(directory, uuid.NewString()+".json")
	garbagePath := filepath.Join(directory, uuid.NewString()+".json")
	oversizedPath := filepath.Join(directory, uuid.NewString()+".json")
	notesPath := filepath.Join(directory, "notes.txt")
	for path, content := range map[string][]byte{
		misnamedPath:  misnamedData,
		garbagePath:   []byte("{not json"),
		oversizedPath: bytes.Repeat([]byte(" "), maximumUploadMetadataFileSize+1),
		notesPath:     []byte("not metadata"),
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}

	loaded, err := manager.readServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}

	if len(loaded) != 1 || loaded[0].session.ID != valid.ID {
		t.Fatalf("expected only the valid session to load, got %+v", loaded)
	}
	for _, path := range []string{misnamedPath, garbagePath, oversizedPath, manager.sessionPath(s.ID(), expired.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, got %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{manager.sessionPath(s.ID(), valid.ID), notesPath, filepath.Join(directory, "nested")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to be left alone: %v", filepath.Base(path), err)
		}
	}
}

// TestReadServerSessionsWithoutADirectoryIsEmpty treats a server that never
// uploaded anything as having no sessions rather than as an error.
func TestReadServerSessionsWithoutADirectoryIsEmpty(t *testing.T) {
	s, manager := newUploadTestServer(t)

	loaded, err := manager.readServerSessions(s)

	if err != nil || len(loaded) != 0 {
		t.Fatalf("expected no sessions and no error, got %+v err=%v", loaded, err)
	}
}

// TestReconcileRecoversACompletionInterruptedAfterThePublish handles a
// restart between the atomic rename and the final metadata write: the
// published file matches the fingerprint, so the session is completed and
// the recovery is written back to disk.
func TestReconcileRecoversACompletionInterruptedAfterThePublish(t *testing.T) {
	s, manager := newUploadTestServer(t)
	content := []byte("published bytes")
	session := newUploadTestSession(s, "world.tar", content)
	session.State = uploadSessionCompleting
	persistUploadTestSession(t, manager, session)
	if err := s.Filesystem().Writefile(session.Target, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	recovered, err := manager.reconcile(s, session)
	if err != nil {
		t.Fatal(err)
	}

	if !recovered.Complete() || recovered.Offset != session.Size {
		t.Fatalf("expected the session to be completed, got %+v", recovered)
	}
	if stored := readUploadTestSession(t, manager, session); !stored.Complete() {
		t.Fatalf("expected the recovery to be persisted, got state %q", stored.State)
	}
}

// TestReconcileRestartsWhenThePublishedFileDoesNotMatch refuses to trust a
// destination file of the right size but the wrong content, and restarts the
// upload from the beginning instead.
func TestReconcileRestartsWhenThePublishedFileDoesNotMatch(t *testing.T) {
	s, manager := newUploadTestServer(t)
	content := []byte("published bytes")
	session := newUploadTestSession(s, "world.tar", content)
	session.State = uploadSessionCompleting
	if err := s.Filesystem().Writefile(session.Target, bytes.NewReader([]byte("different bytes"))); err != nil {
		t.Fatal(err)
	}

	recovered, err := manager.reconcile(s, session)
	if err != nil {
		t.Fatal(err)
	}

	if recovered.State != uploadSessionActive || recovered.Offset != 0 {
		t.Fatalf("expected the upload to restart, got %+v", recovered)
	}
}

// TestReconcileDerivesTheOffsetFromThePartial trusts the bytes on disk over
// anything the client claims, and refuses a partial that already exceeds the
// declared size.
func TestReconcileDerivesTheOffsetFromThePartial(t *testing.T) {
	s, manager := newUploadTestServer(t)
	session := newUploadTestSession(s, "world.tar", []byte("0123456789"))

	if err := s.Filesystem().Writefile(session.Partial, strings.NewReader("0123")); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.reconcile(s, session)
	if err != nil || resumed.Offset != 4 {
		t.Fatalf("expected the offset to come from the partial, got offset=%d err=%v", resumed.Offset, err)
	}

	if err := s.Filesystem().Writefile(session.Partial, strings.NewReader("0123456789x")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.reconcile(s, session); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("expected an oversized partial to be refused, got %v", err)
	}
}

// TestPurgeOrphanServersRemovesOnlyUnknownDirectories deletes metadata for
// servers the panel no longer knows about and nothing else.
func TestPurgeOrphanServersRemovesOnlyUnknownDirectories(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	known := filepath.Join(manager.root, uuid.NewString())
	orphan := filepath.Join(manager.root, uuid.NewString())
	notes := filepath.Join(manager.root, "notes.txt")
	for _, directory := range []string{known, orphan} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(notes, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := manager.PurgeOrphanServers([]string{filepath.Base(known)}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("expected the orphan directory to be removed, got %v", err)
	}
	for _, path := range []string{known, notes} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to survive: %v", filepath.Base(path), err)
		}
	}
	if err := NewUploadManager(filepath.Join(t.TempDir(), "missing")).PurgeOrphanServers(nil); err != nil {
		t.Fatalf("expected a missing root to be a no-op, got %v", err)
	}
}

// TestIndexSessionAccountingReplacesAnEntryWithoutDoubleCounting exercises
// the O(1) cap counters through an update, a completion, and a removal, since
// a drift in any of them silently changes how many uploads a node admits.
func TestIndexSessionAccountingReplacesAnEntryWithoutDoubleCounting(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	s := &Server{}
	s.cfg.Uuid = uuid.NewString()
	session := newUploadTestSession(s, "world.tar", []byte("data"))
	userKey := uploadUserKey(session.ServerUUID, session.UserUUID)

	manager.indexSessionLocked(session, 100)
	manager.indexSessionLocked(session, 150)
	if manager.totalStored != 1 || manager.totalMetadataSize != 150 || manager.activeNode != 1 ||
		manager.activeServers[s.ID()] != 1 || manager.activeUsers[userKey] != 1 || manager.targets[s.ID()]["world.tar"] != session.ID {
		t.Fatalf("expected one active session counted once, got stored=%d bytes=%d active=%d", manager.totalStored, manager.totalMetadataSize, manager.activeNode)
	}

	session.State = uploadSessionComplete
	manager.indexSessionLocked(session, 120)
	if manager.totalStored != 1 || manager.totalMetadataSize != 120 || manager.activeNode != 0 ||
		manager.activeServers[s.ID()] != 0 || manager.activeUsers[userKey] != 0 || manager.targets[s.ID()]["world.tar"] != "" {
		t.Fatalf("expected completion to release the active counters, got stored=%d bytes=%d active=%d target=%q", manager.totalStored, manager.totalMetadataSize, manager.activeNode, manager.targets[s.ID()]["world.tar"])
	}

	manager.removeIndexedSessionLocked(session)
	manager.removeIndexedSessionLocked(session)
	if manager.totalStored != 0 || manager.totalMetadataSize != 0 || len(manager.sessions[s.ID()]) != 0 {
		t.Fatalf("expected removal to zero the accounting, got stored=%d bytes=%d sessions=%d", manager.totalStored, manager.totalMetadataSize, len(manager.sessions[s.ID()]))
	}
}

// TestCanIndexSessionRefusesASecondActiveUploadForTheSameTarget keeps two
// writers off one destination while letting a completed record and a
// different destination through, and refuses everything for a server being purged.
func TestCanIndexSessionRefusesASecondActiveUploadForTheSameTarget(t *testing.T) {
	manager := NewUploadManager(t.TempDir())
	s := &Server{}
	s.cfg.Uuid = uuid.NewString()
	first := newUploadTestSession(s, "world.tar", []byte("data"))
	manager.indexSessionLocked(first, 100)

	second := newUploadTestSession(s, "world.tar", []byte("data"))
	if manager.canIndexSessionLocked(second, 100) {
		t.Fatal("a second active upload to the same target must be refused")
	}
	elsewhere := newUploadTestSession(s, "other.tar", []byte("data"))
	if !manager.canIndexSessionLocked(elsewhere, 100) {
		t.Fatal("an upload to a free target must be allowed")
	}
	completed := second
	completed.State = uploadSessionComplete
	if !manager.canIndexSessionLocked(completed, 100) {
		t.Fatal("a completed record must not count against the active caps")
	}

	manager.purgingServers[s.ID()] = true
	if manager.canIndexSessionLocked(elsewhere, 100) {
		t.Fatal("nothing may be indexed for a server that is being purged")
	}
}

// TestEnsureDurableDirectoryCreatesAncestorsAndRejectsFiles builds the whole
// missing chain with the requested mode and refuses a path that is a file.
func TestEnsureDurableDirectoryCreatesAncestorsAndRejectsFiles(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")

	if err := ensureDurableDirectory(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nested)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected a 0700 directory, got info=%v err=%v", info, err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureDurableDirectory(file, 0o700); err == nil {
		t.Fatal("expected a file in place of the directory to be an error")
	}
}

// TestRemovingMissingUploadFilesIsNotAnError keeps cleanup idempotent for
// both the metadata file and the partial, since either may already be gone
// after an interrupted removal.
func TestRemovingMissingUploadFilesIsNotAnError(t *testing.T) {
	s, _ := newUploadTestServer(t)

	if err := removeMetadataFile(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("expected a missing metadata file to be ignored, got %v", err)
	}
	if err := deleteUploadPartial(s, ".wings-upload-missing.part"); err != nil {
		t.Fatalf("expected a missing partial to be ignored, got %v", err)
	}
}
