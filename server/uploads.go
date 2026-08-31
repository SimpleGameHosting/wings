package server

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/ufs"
)

const (
	resumableUploadSessionLifetime          = 24 * time.Hour
	completedResumableUploadSessionLifetime = 15 * time.Minute
	maximumUploadMetadataFileSize           = 64 * 1024
)

var (
	ErrUploadConflict            = errors.New("an active upload already targets this file")
	ErrUploadChecksumMismatch    = errors.New("uploaded content does not match its SHA-256 fingerprint")
	ErrUploadFingerprintMismatch = errors.New("upload fingerprint does not match the session")
	ErrUploadIncomplete          = errors.New("upload is incomplete")
	ErrUploadLimitReached        = errors.New("resumable upload session limit reached")
	ErrUploadNoProgress          = errors.New("upload request did not contain any new bytes")
	ErrUploadNotFound            = errors.New("upload session was not found")
	ErrUploadTooLarge            = errors.New("upload chunk exceeds the declared upload length")
)

type uploadSessionState string

const (
	uploadSessionActive     uploadSessionState = "active"
	uploadSessionCompleting uploadSessionState = "completing"
	uploadSessionComplete   uploadSessionState = "complete"
)

// UploadOffsetError reports the server-confirmed offset after a client sends a stale position.
type UploadOffsetError struct {
	Expected int64
}

// Error returns a stable description suitable for internal logs.
func (e *UploadOffsetError) Error() string {
	return fmt.Sprintf("upload offset does not match: expected %d", e.Expected)
}

// UploadSession is the persistent identity and current status of one resumable file upload.
type UploadSession struct {
	ID          string             `json:"id"`
	ServerUUID  string             `json:"server_uuid"`
	UserUUID    string             `json:"user_uuid"`
	Target      string             `json:"target"`
	Partial     string             `json:"partial"`
	Fingerprint string             `json:"fingerprint"`
	Size        int64              `json:"size"`
	State       uploadSessionState `json:"state"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Offset      int64              `json:"-"`
}

// Complete reports whether Wings has atomically published the uploaded file.
func (s UploadSession) Complete() bool {
	return s.State == uploadSessionComplete
}

// ExpiresAt returns the exact time at which Wings will stop accepting this session.
func (s UploadSession) ExpiresAt() time.Time {
	if s.Complete() {
		return s.UpdatedAt.Add(completedResumableUploadSessionLifetime)
	}
	return s.UpdatedAt.Add(resumableUploadSessionLifetime)
}

// Expired reports whether a session has passed the retention period for its current state.
func (s UploadSession) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt())
}

// UploadManager owns bounded, durable upload state and serializes changes to each target file.
type UploadManager struct {
	root   string
	limits config.ResumableUploadConfiguration
	locks  uploadTargetLocks
	guards uploadRequestGuards

	lifecycleMu sync.RWMutex
	createMu    sync.Mutex
	indexMu     sync.RWMutex

	sessions          map[string]map[string]UploadSession
	targets           map[string]map[string]string
	metadataSizes     map[string]int64
	loadedServers     map[string]bool
	purgingServers    map[string]bool
	totalMetadataSize int64
	totalStored       int
	activeNode        int
	activeServers     map[string]int
	activeUsers       map[string]int
}

// NewUploadManager creates a resumable upload manager rooted outside customer server volumes.
func NewUploadManager(root string) *UploadManager {
	return &UploadManager{
		root:           root,
		limits:         config.Get().Api.ResumableUploads,
		locks:          uploadTargetLocks{entries: make(map[string]*uploadTargetLock)},
		guards:         newUploadRequestGuards(),
		sessions:       make(map[string]map[string]UploadSession),
		targets:        make(map[string]map[string]string),
		metadataSizes:  make(map[string]int64),
		loadedServers:  make(map[string]bool),
		purgingServers: make(map[string]bool),
		activeServers:  make(map[string]int),
		activeUsers:    make(map[string]int),
	}
}

// InitializeServer loads and validates one server's durable upload metadata exactly once.
func (m *UploadManager) InitializeServer(server *Server) error {
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()

	m.createMu.Lock()

	m.indexMu.RLock()
	loaded := m.loadedServers[server.ID()]
	m.indexMu.RUnlock()
	if loaded {
		m.createMu.Unlock()
		return m.CleanupExpired(server)
	}
	defer m.createMu.Unlock()

	sessions, err := m.readServerSessions(server)
	if err != nil {
		return err
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].session.UpdatedAt.After(sessions[j].session.UpdatedAt)
	})
	if err := m.indexServerSessions(server, sessions, m.removeSessionFiles); err != nil {
		return err
	}

	m.indexMu.Lock()
	m.loadedServers[server.ID()] = true
	delete(m.purgingServers, server.ID())
	m.indexMu.Unlock()
	return nil
}

// indexServerSessions applies startup caps atomically so failed cleanup cannot leak counters.
func (m *UploadManager) indexServerSessions(
	server *Server,
	sessions []loadedUploadSession,
	removeSessionFiles func(*Server, UploadSession) error,
) error {
	indexed := make([]UploadSession, 0, len(sessions))
	committed := false
	defer func() {
		if committed {
			return
		}
		m.indexMu.Lock()
		for _, session := range indexed {
			m.removeIndexedSessionLocked(session)
		}
		m.indexMu.Unlock()
	}()

	for _, candidate := range sessions {
		session := candidate.session
		m.indexMu.Lock()
		remove := !m.canIndexSessionLocked(session, candidate.size)
		if !remove {
			m.indexSessionLocked(session, candidate.size)
			indexed = append(indexed, session)
		}
		m.indexMu.Unlock()

		if remove {
			if err := removeSessionFiles(server, session); err != nil {
				return err
			}
		}
	}
	committed = true
	return nil
}

// Create starts a new upload session while leaving any existing destination file untouched.
func (m *UploadManager) Create(server *Server, userUUID, uploadID, target, fingerprint string, size int64) (UploadSession, error) {
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()

	unlock := m.locks.lock(uploadTargetLockKey(server.ID(), target))
	defer unlock()

	// First, serialize session creation so hard node-wide caps cannot be raced...
	m.createMu.Lock()
	defer m.createMu.Unlock()
	if existing, ok := m.indexedSession(server.ID(), uploadID); ok {
		if existing.UserUUID != userUUID || existing.Target != target || existing.Fingerprint != fingerprint || existing.Size != size {
			return UploadSession{}, ErrUploadConflict
		}
		if existing.Expired(time.Now().UTC()) {
			if err := m.removeSession(server, existing); err != nil {
				return UploadSession{}, err
			}
		} else {
			return m.reconcile(server, existing)
		}
	}
	if err := m.prepareTarget(server, target); err != nil {
		return UploadSession{}, err
	}

	now := time.Now().UTC()
	session := UploadSession{
		ID:          uploadID,
		ServerUUID:  server.ID(),
		UserUUID:    userUUID,
		Target:      target,
		Fingerprint: fingerprint,
		Size:        size,
		State:       uploadSessionActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	session.Partial = expectedUploadPartial(session)
	data, err := json.Marshal(session)
	if err != nil {
		return UploadSession{}, err
	}

	m.indexMu.RLock()
	allowed := m.canIndexSessionLocked(session, int64(len(data)))
	m.indexMu.RUnlock()
	if !allowed {
		return UploadSession{}, ErrUploadLimitReached
	}
	if err := m.persistSession(session, data); err != nil {
		return UploadSession{}, err
	}

	m.indexMu.Lock()
	m.indexSessionLocked(session, int64(len(data)))
	m.indexMu.Unlock()
	return session, nil
}

// CleanupExpired removes stale metadata and unfinished partial files for one loaded server.
func (m *UploadManager) CleanupExpired(server *Server) error {
	m.indexMu.RLock()
	indexed := make([]UploadSession, 0, len(m.sessions[server.ID()]))
	for _, session := range m.sessions[server.ID()] {
		indexed = append(indexed, session)
	}
	m.indexMu.RUnlock()

	now := time.Now().UTC()
	for _, session := range indexed {
		if !session.Expired(now) && server.Filesystem().IsIgnored(session.Target) == nil {
			continue
		}
		unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
		current, ok := m.indexedSession(server.ID(), session.ID)
		if ok && (current.Expired(time.Now().UTC()) || server.Filesystem().IsIgnored(current.Target) != nil) {
			if err := m.removeSession(server, current); err != nil {
				unlock()
				return err
			}
		}
		unlock()
	}
	return nil
}

// StartMaintenance periodically expires upload state until Wings shuts down.
func (m *UploadManager) StartMaintenance(ctx context.Context, servers func() []*Server) {
	interval := time.Duration(m.limits.CleanupIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.cleanupRequestGuards(time.Now().UTC())
				for _, server := range servers() {
					if err := m.CleanupExpired(server); err != nil {
						log.WithFields(log.Fields{"server": server.ID(), "error": err}).Warn("failed to clean resumable upload state")
					}
				}
			}
		}
	}()
}

// PurgeOrphanServers removes metadata directories that no longer map to a configured server.
func (m *UploadManager) PurgeOrphanServers(serverUUIDs []string) error {
	allowed := make(map[string]struct{}, len(serverUUIDs))
	for _, serverUUID := range serverUUIDs {
		allowed[serverUUID] = struct{}{}
	}
	entries, err := os.ReadDir(m.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := allowed[entry.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.root, entry.Name())); err != nil {
			return err
		}
	}
	return syncDirectory(m.root)
}

// PurgeServer permanently removes all resumable state belonging to a deleted server.
func (m *UploadManager) PurgeServer(server *Server) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.indexMu.Lock()
	m.purgingServers[server.ID()] = true
	indexed := make([]UploadSession, 0, len(m.sessions[server.ID()]))
	for _, session := range m.sessions[server.ID()] {
		indexed = append(indexed, session)
	}
	m.indexMu.Unlock()

	for _, session := range indexed {
		unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
		if current, ok := m.indexedSessionForPurge(server.ID(), session.ID); ok {
			if err := m.removeSession(server, current); err != nil {
				unlock()
				return err
			}
		}
		unlock()
	}
	if err := os.RemoveAll(m.serverDirectory(server.ID())); err != nil {
		return err
	}
	if err := syncDirectory(m.root); err != nil && !os.IsNotExist(err) {
		return err
	}

	m.indexMu.Lock()
	delete(m.sessions, server.ID())
	delete(m.targets, server.ID())
	delete(m.loadedServers, server.ID())
	m.indexMu.Unlock()
	return nil
}

// Status returns the durable offset and completion state for an authorized upload session.
func (m *UploadManager) Status(server *Server, userUUID, uploadID, fingerprint string) (UploadSession, error) {
	session, err := m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return UploadSession{}, err
	}

	unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
	defer unlock()

	// Next, reload under the target lock and reconcile an interrupted atomic completion...
	session, err = m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return UploadSession{}, err
	}
	if err := server.Filesystem().IsIgnored(session.Target); err != nil {
		return UploadSession{}, err
	}
	return m.reconcile(server, session)
}

// WriteChunk appends a request body at the confirmed offset and publishes the target when complete.
func (m *UploadManager) WriteChunk(
	ctx context.Context,
	server *Server,
	userUUID string,
	uploadID string,
	fingerprint string,
	offset int64,
	contentLength int64,
	complete bool,
	reader io.Reader,
) (UploadSession, bool, error) {
	session, err := m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return UploadSession{}, false, err
	}

	unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
	defer unlock()

	// Next, establish the authoritative on-disk state while no competing chunk can change it...
	session, err = m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return UploadSession{}, false, err
	}
	if err := server.Filesystem().IsIgnored(session.Target); err != nil {
		return UploadSession{}, false, err
	}
	session, err = m.reconcile(server, session)
	if err != nil {
		return UploadSession{}, false, err
	}
	if offset != session.Offset {
		return session, false, &UploadOffsetError{Expected: session.Offset}
	}
	if session.Complete() {
		return session, false, nil
	}

	remaining := session.Size - session.Offset
	if contentLength > remaining {
		return session, false, ErrUploadTooLarge
	}
	if contentLength == 0 && remaining > 0 {
		return session, false, ErrUploadNoProgress
	}

	// Next, stream the chunk into the private partial file with filesystem quota enforcement...
	file, err := server.Filesystem().Touch(session.Partial, ufs.O_RDWR)
	if err != nil {
		return session, false, err
	}
	if err := server.Filesystem().Chown(session.Partial); err != nil {
		return session, false, errors.Join(err, file.Close())
	}
	if _, err := file.Seek(session.Offset, io.SeekStart); err != nil {
		return session, false, errors.Join(err, file.Close())
	}

	writer := &boundedUploadWriter{writer: file, remaining: remaining}
	written, copyErr := io.Copy(writer, &uploadContextReader{ctx: ctx, reader: reader})
	if errors.Is(copyErr, ErrUploadTooLarge) {
		if truncateErr := file.Truncate(session.Offset); truncateErr != nil {
			copyErr = errors.Join(copyErr, truncateErr)
		} else {
			copyErr = errors.Join(copyErr, unix.Fdatasync(int(file.Fd())))
		}
		written = 0
	}
	if written > 0 || (complete && remaining == 0) {
		copyErr = errors.Join(copyErr, unix.Fdatasync(int(file.Fd())))
	}
	closeErr := file.Close()
	if written == 0 && copyErr == nil && remaining > 0 {
		return session, false, errors.Join(ErrUploadNoProgress, closeErr)
	}
	if written > 0 {
		session.Offset += written
		session.UpdatedAt = time.Now().UTC()
		if saveErr := m.saveExisting(session); saveErr != nil {
			return session, false, errors.Join(copyErr, closeErr, saveErr)
		}
	}
	if copyErr != nil || closeErr != nil {
		return session, false, errors.Join(copyErr, closeErr)
	}
	if !complete {
		return session, false, nil
	}
	if session.Offset != session.Size {
		return session, false, ErrUploadIncomplete
	}
	if err := verifyUploadFingerprint(ctx, server, session.Partial, session); err != nil {
		if errors.Is(err, ErrUploadChecksumMismatch) {
			if cleanupErr := m.removeSession(server, session); cleanupErr != nil {
				return session, false, cleanupErr
			}
		}
		return session, false, err
	}
	if err := server.Filesystem().IsIgnored(session.Target); err != nil {
		return session, false, err
	}

	// Finally, durably record the transition before and after atomic destination replacement...
	session.State = uploadSessionCompleting
	if err := m.saveExisting(session); err != nil {
		return session, false, err
	}
	if err := server.Filesystem().Replace(session.Partial, session.Target); err != nil {
		session.State = uploadSessionActive
		return session, false, errors.Join(err, m.saveExisting(session))
	}
	if err := server.Filesystem().UnixFS().SyncParent(session.Target); err != nil {
		return session, false, err
	}
	session.State = uploadSessionComplete
	session.Offset = session.Size
	session.UpdatedAt = time.Now().UTC()
	if err := m.saveExisting(session); err != nil {
		return session, false, err
	}

	return session, true, nil
}

// verifyUploadFingerprint hashes a completed upload path before Wings trusts or publishes it.
func verifyUploadFingerprint(ctx context.Context, server *Server, path string, session UploadSession) error {
	file, err := server.Filesystem().UnixFS().Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, &uploadContextReader{ctx: ctx, reader: file})
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if written != session.Size || fmt.Sprintf("%x", hash.Sum(nil)) != session.Fingerprint {
		return ErrUploadChecksumMismatch
	}
	return nil
}

// Cancel removes a session and any unfinished partial without touching the destination.
func (m *UploadManager) Cancel(server *Server, userUUID, uploadID, fingerprint string) error {
	session, err := m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return err
	}

	unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
	defer unlock()
	session, err = m.loadAuthorized(server, userUUID, uploadID, fingerprint)
	if err != nil {
		return err
	}
	return m.removeSession(server, session)
}

// prepareTarget removes an expired writer and rejects a current upload for the same destination.
func (m *UploadManager) prepareTarget(server *Server, target string) error {
	m.indexMu.RLock()
	if m.purgingServers[server.ID()] || !m.loadedServers[server.ID()] {
		m.indexMu.RUnlock()
		return ErrUploadNotFound
	}
	uploadID := m.targets[server.ID()][target]
	session, exists := m.sessions[server.ID()][uploadID]
	m.indexMu.RUnlock()
	if !exists {
		return nil
	}
	if !session.Expired(time.Now().UTC()) {
		return ErrUploadConflict
	}
	return m.removeSession(server, session)
}

// loadAuthorized validates an indexed session identity without revealing ownership mismatches.
func (m *UploadManager) loadAuthorized(server *Server, userUUID, uploadID, fingerprint string) (UploadSession, error) {
	if _, err := uuid.Parse(uploadID); err != nil {
		return UploadSession{}, ErrUploadNotFound
	}
	session, ok := m.indexedSession(server.ID(), uploadID)
	if !ok || session.ServerUUID != server.ID() || session.UserUUID != userUUID {
		return UploadSession{}, ErrUploadNotFound
	}
	if session.Fingerprint != fingerprint {
		return UploadSession{}, ErrUploadFingerprintMismatch
	}
	if session.Expired(time.Now().UTC()) {
		return UploadSession{}, ErrUploadNotFound
	}
	return session, nil
}

// reconcile recovers completion state and derives the current offset from the partial file.
func (m *UploadManager) reconcile(server *Server, session UploadSession) (UploadSession, error) {
	if session.Complete() {
		session.Offset = session.Size
		return session, nil
	}

	info, err := server.Filesystem().UnixFS().Stat(session.Partial)
	if err == nil {
		if !info.Mode().IsRegular() {
			return UploadSession{}, fmt.Errorf("upload partial is not a regular file")
		}
		if info.Size() > session.Size {
			return UploadSession{}, ErrUploadTooLarge
		}
		session.Offset = info.Size()
		return session, nil
	}
	if !errors.Is(err, ufs.ErrNotExist) {
		return UploadSession{}, err
	}

	// A missing partial in the completing state means the atomic rename may have won before a restart...
	if session.State == uploadSessionCompleting {
		target, targetErr := server.Filesystem().UnixFS().Stat(session.Target)
		if targetErr == nil && target.Mode().IsRegular() && target.Size() == session.Size {
			fingerprintErr := verifyUploadFingerprint(context.Background(), server, session.Target, session)
			if fingerprintErr == nil {
				session.State = uploadSessionComplete
				session.Offset = session.Size
				session.UpdatedAt = time.Now().UTC()
				if err := m.saveExisting(session); err != nil {
					return UploadSession{}, err
				}
				return session, nil
			}
			if !errors.Is(fingerprintErr, ErrUploadChecksumMismatch) {
				return UploadSession{}, fingerprintErr
			}
		}
	}

	session.State = uploadSessionActive
	session.Offset = 0
	return session, nil
}

// readServerSessions decodes bounded metadata files without trusting their stored paths.
func (m *UploadManager) readServerSessions(server *Server) ([]loadedUploadSession, error) {
	directory := m.serverDirectory(server.ID())
	handle, err := os.Open(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	maximumCandidates := m.limits.MaxStoredSessionsNode
	if maximumCandidates <= 0 {
		maximumCandidates = 16384
	}
	maximumCandidateBytes := m.limits.MaxMetadataBytes
	if maximumCandidateBytes <= 0 {
		maximumCandidateBytes = 64 * 1024 * 1024
	}
	candidates := make(loadedUploadSessionHeap, 0, min(maximumCandidates, 128))
	heap.Init(&candidates)
	var candidateBytes int64
	now := time.Now().UTC()
	for {
		entries, readErr := handle.ReadDir(128)
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() || info.Size() > maximumUploadMetadataFileSize {
				if err := removeMetadataFile(path); err != nil {
					return nil, err
				}
				continue
			}
			data, fileErr := os.ReadFile(path)
			if fileErr != nil {
				if os.IsNotExist(fileErr) {
					continue
				}
				return nil, fileErr
			}
			var session UploadSession
			if json.Unmarshal(data, &session) != nil || !validStoredUploadSession(server.ID(), entry.Name(), session, m.limits) {
				if err := removeMetadataFile(path); err != nil {
					return nil, err
				}
				continue
			}
			if session.Expired(now) || server.Filesystem().IsIgnored(session.Target) != nil {
				if err := m.removeSessionFiles(server, session); err != nil {
					return nil, err
				}
				continue
			}
			candidate := loadedUploadSession{session: session, size: int64(len(data))}
			if err := m.retainServerSessionCandidate(server, &candidates, &candidateBytes, candidate, maximumCandidates, maximumCandidateBytes); err != nil {
				return nil, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return []loadedUploadSession(candidates), nil
}

// retainServerSessionCandidate keeps the newest valid metadata within restart memory limits.
func (m *UploadManager) retainServerSessionCandidate(
	server *Server,
	candidates *loadedUploadSessionHeap,
	candidateBytes *int64,
	candidate loadedUploadSession,
	maximumCandidates int,
	maximumCandidateBytes int64,
) error {
	heap.Push(candidates, candidate)
	*candidateBytes += candidate.size
	for candidates.Len() > maximumCandidates || *candidateBytes > maximumCandidateBytes {
		oldest := heap.Pop(candidates).(loadedUploadSession)
		*candidateBytes -= oldest.size
		if err := m.removeSessionFiles(server, oldest.session); err != nil {
			return err
		}
	}
	return nil
}

// persistSession atomically commits metadata and its directory entry to stable storage.
func (m *UploadManager) persistSession(session UploadSession, data []byte) (returnErr error) {
	directory := m.serverDirectory(session.ServerUUID)
	if err := ensureDurableDirectory(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".upload-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryPath); err != nil && !os.IsNotExist(err) {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, m.sessionPath(session.ServerUUID, session.ID)); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// saveExisting persists a known session and then updates its in-memory index entry.
func (m *UploadManager) saveExisting(session UploadSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if err := m.persistSession(session, data); err != nil {
		return err
	}
	m.indexMu.Lock()
	m.indexSessionLocked(session, int64(len(data)))
	m.indexMu.Unlock()
	return nil
}

// removeSession deletes durable files before making the session unreachable in memory.
func (m *UploadManager) removeSession(server *Server, session UploadSession) error {
	if err := m.removeSessionFiles(server, session); err != nil {
		return err
	}
	m.indexMu.Lock()
	m.removeIndexedSessionLocked(session)
	m.indexMu.Unlock()
	return nil
}

// removeSessionFiles removes a partial file and metadata with durable directory updates.
func (m *UploadManager) removeSessionFiles(server *Server, session UploadSession) error {
	if !session.Complete() {
		if err := deleteUploadPartial(server, session.Partial); err != nil {
			return err
		}
	}
	return removeMetadataFile(m.sessionPath(session.ServerUUID, session.ID))
}

// canIndexSessionLocked checks all configured session and metadata caps.
func (m *UploadManager) canIndexSessionLocked(session UploadSession, metadataSize int64) bool {
	if m.purgingServers[session.ServerUUID] {
		return false
	}
	if m.limitReached(m.totalStored, m.limits.MaxStoredSessionsNode) ||
		m.byteLimitReached(m.totalMetadataSize, metadataSize, m.limits.MaxMetadataBytes) {
		return false
	}
	if session.Complete() {
		return true
	}
	return !m.limitReached(m.activeNode, m.limits.MaxActiveSessionsNode) &&
		!m.limitReached(m.activeServers[session.ServerUUID], m.limits.MaxActiveSessionsPerServer) &&
		!m.limitReached(m.activeUsers[uploadUserKey(session.ServerUUID, session.UserUUID)], m.limits.MaxActiveSessionsPerUser) &&
		m.targets[session.ServerUUID][session.Target] == ""
}

// indexSessionLocked adds or replaces one session while maintaining O(1) cap counters.
func (m *UploadManager) indexSessionLocked(session UploadSession, metadataSize int64) {
	if m.sessions[session.ServerUUID] == nil {
		m.sessions[session.ServerUUID] = make(map[string]UploadSession)
	}
	if m.targets[session.ServerUUID] == nil {
		m.targets[session.ServerUUID] = make(map[string]string)
	}
	key := uploadSessionKey(session.ServerUUID, session.ID)
	previous, exists := m.sessions[session.ServerUUID][session.ID]
	if exists {
		m.removeSessionCountersLocked(previous)
		m.totalMetadataSize -= m.metadataSizes[key]
	} else {
		m.totalStored++
	}
	m.sessions[session.ServerUUID][session.ID] = session
	m.metadataSizes[key] = metadataSize
	m.totalMetadataSize += metadataSize
	m.addSessionCountersLocked(session)
}

// removeIndexedSessionLocked drops one session and its accounting counters.
func (m *UploadManager) removeIndexedSessionLocked(session UploadSession) {
	indexed, exists := m.sessions[session.ServerUUID][session.ID]
	if !exists {
		return
	}
	m.removeSessionCountersLocked(indexed)
	delete(m.sessions[session.ServerUUID], session.ID)
	key := uploadSessionKey(session.ServerUUID, session.ID)
	m.totalMetadataSize -= m.metadataSizes[key]
	delete(m.metadataSizes, key)
	m.totalStored--
}

// addSessionCountersLocked records active ownership and destination counters.
func (m *UploadManager) addSessionCountersLocked(session UploadSession) {
	if session.Complete() {
		return
	}
	m.activeNode++
	m.activeServers[session.ServerUUID]++
	m.activeUsers[uploadUserKey(session.ServerUUID, session.UserUUID)]++
	m.targets[session.ServerUUID][session.Target] = session.ID
}

// removeSessionCountersLocked reverses active ownership and destination counters.
func (m *UploadManager) removeSessionCountersLocked(session UploadSession) {
	if session.Complete() {
		return
	}
	m.activeNode--
	decrementUploadCounter(m.activeServers, session.ServerUUID)
	userKey := uploadUserKey(session.ServerUUID, session.UserUUID)
	decrementUploadCounter(m.activeUsers, userKey)
	if m.targets[session.ServerUUID][session.Target] == session.ID {
		delete(m.targets[session.ServerUUID], session.Target)
	}
}

// indexedSession returns a copy of one immutable index value.
func (m *UploadManager) indexedSession(serverUUID, uploadID string) (UploadSession, bool) {
	m.indexMu.RLock()
	defer m.indexMu.RUnlock()
	if m.purgingServers[serverUUID] || !m.loadedServers[serverUUID] {
		return UploadSession{}, false
	}
	session, ok := m.sessions[serverUUID][uploadID]
	return session, ok
}

// indexedSessionForPurge reads an indexed value after public access has been disabled.
func (m *UploadManager) indexedSessionForPurge(serverUUID, uploadID string) (UploadSession, bool) {
	m.indexMu.RLock()
	defer m.indexMu.RUnlock()
	session, ok := m.sessions[serverUUID][uploadID]
	return session, ok
}

// serverDirectory returns the private metadata directory for a UUID-backed server.
func (m *UploadManager) serverDirectory(serverUUID string) string {
	return filepath.Join(m.root, serverUUID)
}

// sessionPath returns the metadata filename for a validated server and upload UUID.
func (m *UploadManager) sessionPath(serverUUID, uploadID string) string {
	return filepath.Join(m.serverDirectory(serverUUID), uploadID+".json")
}

// limitReached reports whether adding one more item would exceed a positive cap.
func (m *UploadManager) limitReached(current, maximum int) bool {
	return maximum > 0 && current >= maximum
}

// byteLimitReached reports whether adding metadata would exceed a positive byte cap.
func (m *UploadManager) byteLimitReached(current, added, maximum int64) bool {
	return maximum > 0 && added > maximum-current
}

// loadedUploadSession retains the exact durable metadata size for cap accounting.
type loadedUploadSession struct {
	session UploadSession
	size    int64
}

// loadedUploadSessionHeap orders restart candidates from oldest to newest activity.
type loadedUploadSessionHeap []loadedUploadSession

// Len returns the number of retained restart candidates.
func (h loadedUploadSessionHeap) Len() int {
	return len(h)
}

// Less places the oldest candidate at the root with a stable identity tie-breaker.
func (h loadedUploadSessionHeap) Less(i, j int) bool {
	if h[i].session.UpdatedAt.Equal(h[j].session.UpdatedAt) {
		return h[i].session.ID < h[j].session.ID
	}
	return h[i].session.UpdatedAt.Before(h[j].session.UpdatedAt)
}

// Swap exchanges two restart candidates.
func (h loadedUploadSessionHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

// Push adds one restart candidate to the heap.
func (h *loadedUploadSessionHeap) Push(value any) {
	*h = append(*h, value.(loadedUploadSession))
}

// Pop removes and returns the oldest restart candidate.
func (h *loadedUploadSessionHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = loadedUploadSession{}
	*h = old[:last]
	return value
}

// validStoredUploadSession prevents a tampered metadata file from selecting arbitrary paths.
func validStoredUploadSession(serverUUID, filename string, session UploadSession, limits config.ResumableUploadConfiguration) bool {
	if filename != session.ID+".json" || session.ServerUUID != serverUUID || session.Size < 0 {
		return false
	}
	if _, err := uuid.Parse(session.ID); err != nil {
		return false
	}
	if _, err := uuid.Parse(session.UserUUID); err != nil {
		return false
	}
	if session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() || session.UpdatedAt.Before(session.CreatedAt) {
		return false
	}
	if session.State != uploadSessionActive && session.State != uploadSessionCompleting && session.State != uploadSessionComplete {
		return false
	}
	if clean := filepath.Clean(session.Target); clean == "." || clean == ".." || clean != session.Target || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	if !validStoredUploadTargetLength(session.Target, limits) {
		return false
	}
	if session.Partial != expectedUploadPartial(session) {
		return false
	}
	fingerprint, err := hex.DecodeString(session.Fingerprint)
	return err == nil && len(fingerprint) == sha256.Size
}

// validStoredUploadTargetLength applies configured filesystem limits to restart metadata.
func validStoredUploadTargetLength(target string, limits config.ResumableUploadConfiguration) bool {
	if limits.MaxPathBytes > 0 && len(target) > limits.MaxPathBytes {
		return false
	}
	for _, segment := range strings.Split(target, string(filepath.Separator)) {
		if limits.MaxFilenameBytes > 0 && len(segment) > limits.MaxFilenameBytes {
			return false
		}
	}
	return true
}

// expectedUploadPartial derives the only partial path accepted for a session identity.
func expectedUploadPartial(session UploadSession) string {
	return filepath.Join(filepath.Dir(session.Target), ".wings-upload-"+session.ID+".part")
}

// deleteUploadPartial removes a partial file while preserving filesystem quota accounting.
func deleteUploadPartial(server *Server, partial string) error {
	err := server.Filesystem().Delete(partial)
	if errors.Is(err, ufs.ErrNotExist) {
		return nil
	}
	return err
}

// removeMetadataFile removes manager-owned metadata and durably commits the directory update.
func removeMetadataFile(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// syncDirectory commits prior create, rename, and remove operations within a directory.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// ensureDurableDirectory creates missing ancestors and commits each new directory entry.
func ensureDurableDirectory(path string, mode os.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("metadata path is not a directory: %s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	parent := filepath.Dir(path)
	if parent != path {
		if err := ensureDurableDirectory(parent, mode); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	return syncDirectory(parent)
}

// uploadSessionKey scopes metadata accounting to one server and upload identity.
func uploadSessionKey(serverUUID, uploadID string) string {
	return serverUUID + ":" + uploadID
}

// uploadUserKey scopes active session caps to one server and Panel user.
func uploadUserKey(serverUUID, userUUID string) string {
	return serverUUID + ":" + userUUID
}

// uploadContextReader stops a streaming chunk as soon as the HTTP request is cancelled.
type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read delegates to the request body only while its context remains active.
func (r *uploadContextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

// boundedUploadWriter prevents a chunk from extending beyond the declared total upload length.
type boundedUploadWriter struct {
	writer    io.Writer
	remaining int64
}

// Write accepts only bytes that fit in the remaining declared upload length.
func (w *boundedUploadWriter) Write(buffer []byte) (int, error) {
	if int64(len(buffer)) <= w.remaining {
		written, err := w.writer.Write(buffer)
		w.remaining -= int64(written)
		return written, err
	}
	if w.remaining <= 0 {
		return 0, ErrUploadTooLarge
	}

	written, err := w.writer.Write(buffer[:w.remaining])
	w.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	return written, ErrUploadTooLarge
}

// uploadTargetLocks provides bounded keyed mutexes without retaining completed upload IDs forever.
type uploadTargetLocks struct {
	mu      sync.Mutex
	entries map[string]*uploadTargetLock
}

// uploadTargetLock tracks one mutex and the callers waiting on or holding it.
type uploadTargetLock struct {
	mu         sync.Mutex
	references int
}

// lock acquires a target-specific mutex and returns the only valid release function.
func (l *uploadTargetLocks) lock(key string) func() {
	l.mu.Lock()
	entry := l.entries[key]
	if entry == nil {
		entry = &uploadTargetLock{}
		l.entries[key] = entry
	}
	entry.references++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.references--
		if entry.references == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}

// uploadTargetLockKey scopes a file lock to one server volume and normalized target path.
func uploadTargetLockKey(serverUUID, target string) string {
	return serverUUID + ":" + target
}
