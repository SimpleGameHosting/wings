package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/internal/ufs"
)

const resumableUploadSessionLifetime = 24 * time.Hour

var (
	ErrUploadConflict            = errors.New("an active upload already targets this file")
	ErrUploadChecksumMismatch    = errors.New("uploaded content does not match its SHA-256 fingerprint")
	ErrUploadFingerprintMismatch = errors.New("upload fingerprint does not match the session")
	ErrUploadIncomplete          = errors.New("upload is incomplete")
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

// UploadManager persists upload sessions and serializes changes targeting the same server file.
type UploadManager struct {
	root  string
	locks uploadTargetLocks
}

// NewUploadManager creates a resumable upload manager rooted outside customer server volumes.
func NewUploadManager(root string) *UploadManager {
	return &UploadManager{
		root: root,
		locks: uploadTargetLocks{
			entries: make(map[string]*uploadTargetLock),
		},
	}
}

// Create starts a new upload session while leaving any existing destination file untouched.
func (m *UploadManager) Create(server *Server, userUUID, target, fingerprint string, size int64) (UploadSession, error) {
	if err := m.CleanupExpired(server); err != nil {
		return UploadSession{}, err
	}

	unlock := m.locks.lock(uploadTargetLockKey(server.ID(), target))
	defer unlock()

	// First, reject overlapping writers and remove expired sessions for this destination...
	if err := m.prepareTarget(server, target); err != nil {
		return UploadSession{}, err
	}

	now := time.Now().UTC()
	session := UploadSession{
		ID:          uuid.NewString(),
		ServerUUID:  server.ID(),
		UserUUID:    userUUID,
		Target:      target,
		Fingerprint: fingerprint,
		Size:        size,
		State:       uploadSessionActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	session.Partial = filepath.Join(filepath.Dir(target), ".wings-upload-"+session.ID+".part")

	if err := m.save(session); err != nil {
		return UploadSession{}, err
	}

	return session, nil
}

// CleanupExpired removes stale session metadata and unfinished partial files for one server.
func (m *UploadManager) CleanupExpired(server *Server) error {
	directory := m.serverDirectory(server.ID())
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		session, err := m.load(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if time.Since(session.UpdatedAt) <= resumableUploadSessionLifetime {
			continue
		}

		// Next, re-check expiration under the target lock before deleting durable state...
		unlock := m.locks.lock(uploadTargetLockKey(server.ID(), session.Target))
		current, loadErr := m.load(path)
		if loadErr == nil && time.Since(current.UpdatedAt) > resumableUploadSessionLifetime {
			if !current.Complete() {
				loadErr = deleteUploadPartial(server, current.Partial)
			}
			if loadErr == nil {
				loadErr = removeIfExists(path)
			}
		}
		unlock()
		if loadErr != nil && !os.IsNotExist(loadErr) {
			return loadErr
		}
	}

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

	// Next, stream the chunk into the private partial file with filesystem quota enforcement...
	file, err := server.Filesystem().Touch(session.Partial, ufs.O_RDWR)
	if err != nil {
		return session, false, err
	}
	if _, err := file.Seek(session.Offset, io.SeekStart); err != nil {
		return session, false, errors.Join(err, file.Close())
	}

	writer := &boundedUploadWriter{writer: file, remaining: remaining}
	written, copyErr := io.Copy(writer, &uploadContextReader{ctx: ctx, reader: reader})
	if errors.Is(copyErr, ErrUploadTooLarge) {
		if truncateErr := file.Truncate(session.Offset); truncateErr != nil {
			copyErr = errors.Join(copyErr, truncateErr)
		}
		written = 0
	}
	closeErr := file.Close()
	session.Offset += written
	session.UpdatedAt = time.Now().UTC()

	if saveErr := m.save(session); saveErr != nil {
		return session, false, errors.Join(copyErr, closeErr, saveErr)
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
			if cleanupErr := deleteUploadPartial(server, session.Partial); cleanupErr != nil {
				return session, false, cleanupErr
			}
			if cleanupErr := removeIfExists(m.sessionPath(session.ServerUUID, session.ID)); cleanupErr != nil {
				return session, false, cleanupErr
			}
		}
		return session, false, err
	}

	// Finally, record the transition before and after the atomic destination replacement...
	session.State = uploadSessionCompleting
	if err := m.save(session); err != nil {
		return session, false, err
	}
	if err := server.Filesystem().Replace(session.Partial, session.Target); err != nil {
		session.State = uploadSessionActive
		return session, false, errors.Join(err, m.save(session))
	}
	session.State = uploadSessionComplete
	session.UpdatedAt = time.Now().UTC()
	if err := m.save(session); err != nil {
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

// Cancel removes an unfinished partial file and its session without touching the destination.
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
	if !session.Complete() {
		if err := deleteUploadPartial(server, session.Partial); err != nil {
			return err
		}
	}

	return removeIfExists(m.sessionPath(session.ServerUUID, session.ID))
}

// prepareTarget removes stale sessions and rejects another active upload for the same destination.
func (m *UploadManager) prepareTarget(server *Server, target string) error {
	directory := m.serverDirectory(server.ID())
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		session, err := m.load(filepath.Join(directory, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if session.Target != target || session.Complete() {
			continue
		}
		if time.Since(session.UpdatedAt) <= resumableUploadSessionLifetime {
			return ErrUploadConflict
		}
		if err := deleteUploadPartial(server, session.Partial); err != nil {
			return err
		}
		if err := removeIfExists(m.sessionPath(session.ServerUUID, session.ID)); err != nil {
			return err
		}
	}

	return nil
}

// loadAuthorized validates the durable session identity without revealing ownership mismatches.
func (m *UploadManager) loadAuthorized(server *Server, userUUID, uploadID, fingerprint string) (UploadSession, error) {
	if _, err := uuid.Parse(uploadID); err != nil {
		return UploadSession{}, ErrUploadNotFound
	}

	session, err := m.load(m.sessionPath(server.ID(), uploadID))
	if err != nil {
		if os.IsNotExist(err) {
			return UploadSession{}, ErrUploadNotFound
		}
		return UploadSession{}, err
	}
	if session.ServerUUID != server.ID() || session.UserUUID != userUUID {
		return UploadSession{}, ErrUploadNotFound
	}
	if session.Fingerprint != fingerprint {
		return UploadSession{}, ErrUploadFingerprintMismatch
	}
	if time.Since(session.UpdatedAt) > resumableUploadSessionLifetime {
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
				if err := m.save(session); err != nil {
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

// load decodes one session file from the manager-owned metadata directory.
func (m *UploadManager) load(path string) (UploadSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return UploadSession{}, err
	}

	var session UploadSession
	if err := json.Unmarshal(data, &session); err != nil {
		return UploadSession{}, err
	}
	return session, nil
}

// save atomically persists session state so Wings restarts can safely resume it.
func (m *UploadManager) save(session UploadSession) (returnErr error) {
	directory := m.serverDirectory(session.ServerUUID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(session)
	if err != nil {
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

	return os.Rename(temporaryPath, m.sessionPath(session.ServerUUID, session.ID))
}

// serverDirectory returns the private metadata directory for a UUID-backed server.
func (m *UploadManager) serverDirectory(serverUUID string) string {
	return filepath.Join(m.root, serverUUID)
}

// sessionPath returns the metadata filename for a validated server and upload UUID.
func (m *UploadManager) sessionPath(serverUUID, uploadID string) string {
	return filepath.Join(m.serverDirectory(serverUUID), uploadID+".json")
}

// deleteUploadPartial removes a partial file while preserving filesystem quota accounting.
func deleteUploadPartial(server *Server, partial string) error {
	err := server.Filesystem().Delete(partial)
	if errors.Is(err, ufs.ErrNotExist) {
		return nil
	}
	return err
}

// removeIfExists removes a manager-owned metadata file idempotently.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
