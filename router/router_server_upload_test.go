package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/database"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/router/tokens"
	wserver "github.com/pterodactyl/wings/server"
)

// TestMain initializes the activity database used by successful upload route tests.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "wings-router-tests-")
	if err != nil {
		panic(err)
	}
	previous := config.Get()
	next := *previous
	next.System.RootDirectory = root
	config.Set(&next)
	if err := database.Initialize(); err != nil {
		panic(err)
	}
	config.Set(previous)

	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

const (
	testUploadFingerprint = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	testUploadServerUUID  = "8d2b3f6a-0000-4000-8000-000000000000"
	testUploadUserUUID    = "5a33a971-0000-4000-8000-000000000000"
)

// newUploadTestRouter creates a real server filesystem and the public signed-URL routes used by the Panel.
func newUploadTestRouter(t *testing.T) (*wserver.Server, http.Handler) {
	server, _, handler := newUploadTestState(t)
	return server, handler
}

// newUploadTestState creates configured Wings state and exposes its upload manager to lifecycle tests.
func newUploadTestState(t *testing.T) (*wserver.Server, *wserver.Manager, http.Handler) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.AuthenticationToken = "upload-test-secret"
	next.Token.Token = "upload-test-secret"
	next.Api.UploadLimit = 10
	next.System.RootDirectory = t.TempDir()
	next.System.Data = filepath.Join(next.System.RootDirectory, "volumes")
	next.System.DiskCheckInterval = 150
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() {
		config.Set(previous)
	})

	return newUploadTestStateWithDenylist(t, nil)
}

// newUploadTestRouterFromCurrentConfig recreates Wings state while retaining durable upload files.
func newUploadTestRouterFromCurrentConfig(t *testing.T) (*wserver.Server, http.Handler) {
	return newUploadTestRouterWithDenylist(t, nil)
}

// newUploadTestRouterWithDenylist recreates Wings with a specified live filesystem denylist.
func newUploadTestRouterWithDenylist(t *testing.T, denylist []string) (*wserver.Server, http.Handler) {
	server, _, handler := newUploadTestStateWithDenylist(t, denylist)
	return server, handler
}

// newUploadTestStateWithDenylist exposes the manager for lifecycle tests while retaining real routes and storage.
func newUploadTestStateWithDenylist(t *testing.T, denylist []string) (*wserver.Server, *wserver.Manager, http.Handler) {
	t.Helper()

	client := backupTestRemoteClient{}
	manager := wserver.NewEmptyManager(client)
	settings, err := json.Marshal(map[string]any{
		"uuid": testUploadServerUUID,
		"egg":  map[string]any{"file_denylist": denylist},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Add(server)
	t.Cleanup(server.CtxCancel)

	return server, manager, Configure(manager, client)
}

// uploadActivityCount returns the number of completed file uploads currently persisted by Wings.
func uploadActivityCount(t *testing.T) int64 {
	t.Helper()

	var count int64
	result := database.Instance().Model(&models.Activity{}).
		Where("server = ? AND event = ?", testUploadServerUUID, "server:file.uploaded").
		Count(&count)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	return count
}

// waitForUploadActivity waits for Wings' asynchronous activity writer before test cleanup cancels the server context.
func waitForUploadActivity(t *testing.T, previous int64) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if uploadActivityCount(t) > previous {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected completed upload activity to be persisted")
}

// signedUploadURL returns a fresh signed URL with the same claims issued by the Panel upload endpoint.
func signedUploadURL(t *testing.T, requestURL string, userUUID string, uploadID ...string) string {
	t.Helper()

	payload := tokens.UploadPayload{
		Payload: jwt.Payload{
			// NumericDate is second-precision while Wings records its boot time with
			// nanoseconds, so issue the fixture just ahead of the process boot instant.
			IssuedAt:       jwt.NumericDate(time.Now().Add(time.Second)),
			ExpirationTime: jwt.NumericDate(time.Now().Add(time.Hour)),
			JWTID:          uuid.NewString(),
		},
		Scoped:     tokens.Scoped{Scope: string(tokens.FileUpload)},
		ServerUuid: testUploadServerUUID,
		UserUuid:   userUUID,
		UniqueId:   uuid.NewString(),
	}
	if len(uploadID) > 0 {
		payload.UploadId = uploadID[0]
	}
	signed, err := jwt.Sign(&payload, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("token", string(signed))
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// resumableRequest moves a test credential into the production Authorization header.
func resumableRequest(t *testing.T, method, requestURL string, body io.Reader) *http.Request {
	t.Helper()
	parsed, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	token := parsed.Query().Get("token")
	query := parsed.Query()
	query.Del("token")
	parsed.RawQuery = query.Encode()
	request := httptest.NewRequest(method, parsed.String(), body)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

// createResumableUpload starts an upload session and returns the signed session URL emitted by Wings.
func createResumableUpload(t *testing.T, handler http.Handler, target string, size int64) string {
	return createResumableUploadWithFingerprint(t, handler, target, size, testUploadFingerprint)
}

// createResumableUploadWithFingerprint starts a session with an explicit content identity.
func createResumableUploadWithFingerprint(t *testing.T, handler http.Handler, target string, size int64, fingerprint string) string {
	t.Helper()

	requestURL := signedUploadURL(t, "/upload/file?directory=/&file="+url.QueryEscape(target), testUploadUserUUID)
	request := resumableRequest(t, http.MethodPost, requestURL, nil)
	request.Header.Set("Upload-Complete", "?0")
	request.Header.Set("Upload-Fingerprint", fingerprint)
	request.Header.Set("Upload-ID", uuid.NewString())
	request.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected upload creation to return 201, got %d body %s", recorder.Code, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	if location == "" {
		t.Fatal("expected upload creation to return a session Location")
	}

	uploadID := recorder.Header().Get("Upload-ID")
	if recorder.Header().Get("Upload-Expires") == "" {
		t.Fatal("expected upload creation to return an expiration")
	}
	if parsed, err := url.Parse(location); err != nil || parsed.Query().Get("token") != "" {
		t.Fatalf("expected tokenless session location, got %q", location)
	}
	return signedUploadURL(t, location, testUploadUserUUID, uploadID)
}

// uploadChunk sends one browser-style resumable PATCH request to Wings.
func uploadChunk(t *testing.T, handler http.Handler, location string, offset int64, complete bool, body []byte) *httptest.ResponseRecorder {
	return uploadChunkWithFingerprint(t, handler, location, offset, complete, body, testUploadFingerprint)
}

// uploadChunkWithFingerprint sends one chunk with an explicit content identity.
func uploadChunkWithFingerprint(t *testing.T, handler http.Handler, location string, offset int64, complete bool, body []byte, fingerprint string) *httptest.ResponseRecorder {
	t.Helper()
	request := resumableRequest(t, http.MethodPatch, location, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	completeHeader := "?0"
	if complete {
		completeHeader = "?1"
	}
	request.Header.Set("Upload-Complete", completeHeader)
	request.Header.Set("Upload-Fingerprint", fingerprint)
	request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

// interruptedUploadReader emits bytes received by Wings before simulating a dropped connection.
type interruptedUploadReader struct {
	payload []byte
	sent    bool
}

// TestProtectedUploadReaderRejectsSustainedSlowBodies verifies the direct Wings transport floor.
func TestProtectedUploadReaderRejectsSustainedSlowBodies(t *testing.T) {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	reader := &protectedUploadReader{
		context:       context,
		reader:        bytes.NewReader([]byte("x")),
		timeout:       time.Millisecond,
		minimumRate:   1024,
		windowStarted: time.Now().Add(-time.Second),
	}
	buffer := make([]byte, 1)
	written, err := reader.Read(buffer)
	if written != 1 || !errors.Is(err, errUploadBodyTooSlow) {
		t.Fatalf("expected one preserved byte and slow-body error, got written=%d err=%v", written, err)
	}
}

// TestProtectedUploadReaderResetsHealthyWindows prevents an early burst from subsidizing a slow tail.
func TestProtectedUploadReaderResetsHealthyWindows(t *testing.T) {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	reader := &protectedUploadReader{
		context:       context,
		reader:        bytes.NewReader([]byte("xy")),
		timeout:       time.Millisecond,
		minimumRate:   1024,
		windowStarted: time.Now().Add(-time.Second),
		windowBytes:   2048,
	}
	buffer := make([]byte, 1)
	if written, err := reader.Read(buffer); written != 1 || err != nil {
		t.Fatalf("expected the healthy first window to pass, got written=%d err=%v", written, err)
	}
	if reader.windowBytes != 0 {
		t.Fatalf("expected the healthy window to reset, got %d retained bytes", reader.windowBytes)
	}

	reader.windowStarted = time.Now().Add(-time.Second)
	if written, err := reader.Read(buffer); written != 1 || !errors.Is(err, errUploadBodyTooSlow) {
		t.Fatalf("expected the slow tail to fail independently, got written=%d err=%v", written, err)
	}
}

// Read returns one partial body followed by the transport error that interrupted the request.
func (r *interruptedUploadReader) Read(buffer []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	written := copy(buffer, r.payload)
	return written, io.ErrUnexpectedEOF
}

// TestResumableUploadSurvivesInterruptionAndAtomicallyReplacesTarget reproduces the browser flow customers use after a dropped request.
func TestResumableUploadSurvivesInterruptionAndAtomicallyReplacesTarget(t *testing.T) {
	server, handler := newUploadTestRouter(t)
	if err := server.Filesystem().Write("server.jar", bytes.NewBufferString("old-version"), 11, 0o644); err != nil {
		t.Fatal(err)
	}
	location := createResumableUpload(t, handler, "server.jar", 11)

	interrupted := resumableRequest(t, http.MethodPatch, location, &interruptedUploadReader{payload: []byte("hello ")})
	interrupted.Header.Set("Content-Type", "application/offset+octet-stream")
	interrupted.Header.Set("Upload-Complete", "?0")
	interrupted.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	interrupted.Header.Set("Upload-Offset", "0")
	interruptedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(interruptedRecorder, interrupted)
	if interruptedRecorder.Code < http.StatusBadRequest {
		t.Fatalf("expected interrupted request to fail, got %d body %s", interruptedRecorder.Code, interruptedRecorder.Body.String())
	}
	current, err := os.ReadFile(filepath.Join(server.Filesystem().Path(), "server.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "old-version" {
		t.Fatalf("expected destination to remain untouched until completion, got %q", current)
	}

	server, handler = newUploadTestRouterFromCurrentConfig(t)
	if _, err := server.Filesystem().DiskUsage(false); err != nil {
		t.Fatal(err)
	}

	head := resumableRequest(t, http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	headRecorder := httptest.NewRecorder()
	handler.ServeHTTP(headRecorder, head)
	if headRecorder.Code != http.StatusNoContent || headRecorder.Header().Get("Upload-Offset") != "6" {
		t.Fatalf("expected resumable HEAD offset 6, got %d offset %q", headRecorder.Code, headRecorder.Header().Get("Upload-Offset"))
	}

	activityBefore := uploadActivityCount(t)
	last := uploadChunk(t, handler, location, 6, true, []byte("world"))
	if last.Code != http.StatusNoContent || last.Header().Get("Upload-Complete") != "?1" {
		t.Fatalf("expected completed upload, got %d complete %q body %s", last.Code, last.Header().Get("Upload-Complete"), last.Body.String())
	}
	completed, err := os.ReadFile(filepath.Join(server.Filesystem().Path(), "server.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(completed) != "hello world" {
		t.Fatalf("expected completed destination content, got %q", completed)
	}
	if usage := server.Filesystem().CachedUsage(); usage != int64(len(completed)) {
		t.Fatalf("expected atomic replacement to leave %d quota bytes, got %d", len(completed), usage)
	}
	waitForUploadActivity(t, activityBefore)
}

// TestLegacyMultipartUploadRemainsCompatible protects mixed fleets while the Panel rollout is canaried.
func TestLegacyMultipartUploadRemainsCompatible(t *testing.T) {
	server, handler := newUploadTestRouter(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("legacy upload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	activityBefore := uploadActivityCount(t)
	requestURL := signedUploadURL(t, "/upload/file?directory=/", testUploadUserUUID)
	request := httptest.NewRequest(http.MethodPost, requestURL, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected legacy multipart upload to return 200, got %d body %s", recorder.Code, recorder.Body.String())
	}

	content, err := os.ReadFile(filepath.Join(server.Filesystem().Path(), "legacy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "legacy upload" {
		t.Fatalf("expected legacy multipart content, got %q", content)
	}
	waitForUploadActivity(t, activityBefore)
}

// TestResumableUploadRejectsWrongOffsetAndChecksum ensures retries cannot splice or publish corrupted content.
func TestResumableUploadRejectsWrongOffsetAndChecksum(t *testing.T) {
	server, handler := newUploadTestRouter(t)
	location := createResumableUpload(t, handler, "world.tar", 11)

	recorder := uploadChunk(t, handler, location, 4, false, []byte("world"))
	if recorder.Code != http.StatusConflict || recorder.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("expected offset conflict at zero, got %d offset %q body %s", recorder.Code, recorder.Header().Get("Upload-Offset"), recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.Filesystem().Path(), "world.tar")); !os.IsNotExist(err) {
		t.Fatalf("expected destination not to be created after an offset conflict, got %v", err)
	}

	recorder = uploadChunk(t, handler, location, 0, true, []byte("hello wurld"))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected checksum mismatch to return 422, got %d body %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.Filesystem().Path(), "world.tar")); !os.IsNotExist(err) {
		t.Fatalf("expected corrupted content not to be published, got %v", err)
	}
}

// TestResumableUploadPublishesEmptyFiles verifies the explicit zero-byte completion request.
func TestResumableUploadPublishesEmptyFiles(t *testing.T) {
	const emptyFingerprint = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	server, handler := newUploadTestRouter(t)
	location := createResumableUploadWithFingerprint(t, handler, "empty.txt", 0, emptyFingerprint)

	recorder := uploadChunkWithFingerprint(t, handler, location, 0, true, nil, emptyFingerprint)
	if recorder.Code != http.StatusNoContent || recorder.Header().Get("Upload-Complete") != "?1" {
		t.Fatalf("expected empty upload to complete, got %d complete %q body %s", recorder.Code, recorder.Header().Get("Upload-Complete"), recorder.Body.String())
	}
	info, err := os.Stat(filepath.Join(server.Filesystem().Path(), "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected zero-byte destination, got %d bytes", info.Size())
	}
}

// TestResumableUploadBindsSessionToUserAndFingerprint ensures a leaked session ID cannot be resumed with different upload identity claims.
func TestResumableUploadBindsSessionToUserAndFingerprint(t *testing.T) {
	_, handler := newUploadTestRouter(t)
	location := createResumableUpload(t, handler, "world.tar", 11)

	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	parsed.RawQuery = "upload_id=" + url.QueryEscape(parsed.Query().Get("upload_id"))
	otherUserLocation := signedUploadURL(t, parsed.String(), "6b44ba82-0000-4000-8000-000000000000", parsed.Query().Get("upload_id"))
	request := resumableRequest(t, http.MethodHead, otherUserLocation, nil)
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected another user to receive 404, got %d body %s", recorder.Code, recorder.Body.String())
	}

	request = resumableRequest(t, http.MethodHead, location, nil)
	request.Header.Set("Upload-Fingerprint", "7f44858425cc03d3e797f2707e0f58c3d641f5f6ec7b777109918a7a1f5e4b13")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected a mismatched fingerprint to receive 404, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

// TestResumableUploadRejectsEmptyProgressWithoutExtendingExpiry prevents reusable no-op PATCH requests.
func TestResumableUploadRejectsEmptyProgressWithoutExtendingExpiry(t *testing.T) {
	_, handler := newUploadTestRouter(t)
	location := createResumableUpload(t, handler, "world.tar", 11)

	head := resumableRequest(t, http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	before := httptest.NewRecorder()
	handler.ServeHTTP(before, head)
	if before.Code != http.StatusNoContent {
		t.Fatalf("expected initial status to return 204, got %d", before.Code)
	}

	empty := uploadChunk(t, handler, location, 0, false, nil)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("expected empty PATCH to return 400, got %d body %s", empty.Code, empty.Body.String())
	}

	head = resumableRequest(t, http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, head)
	if after.Code != http.StatusNoContent || after.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("expected offset to remain zero, got %d offset %q", after.Code, after.Header().Get("Upload-Offset"))
	}
	if after.Header().Get("Upload-Expires") != before.Header().Get("Upload-Expires") {
		t.Fatalf("expected empty PATCH not to extend expiration, before %q after %q", before.Header().Get("Upload-Expires"), after.Header().Get("Upload-Expires"))
	}
}

// TestResumableUploadRechecksDenylistAfterRestart prevents old sessions bypassing new Egg rules.
func TestResumableUploadRechecksDenylistAfterRestart(t *testing.T) {
	server, handler := newUploadTestRouter(t)
	location := createResumableUpload(t, handler, "world.tar", 11)
	partial := uploadChunk(t, handler, location, 0, false, []byte("hello"))
	if partial.Code != http.StatusNoContent {
		t.Fatalf("expected partial upload to return 204, got %d body %s", partial.Code, partial.Body.String())
	}

	server.CtxCancel()
	_, restarted := newUploadTestRouterWithDenylist(t, []string{"world.tar"})
	head := resumableRequest(t, http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	recorder := httptest.NewRecorder()
	restarted.ServeHTTP(recorder, head)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected newly denied session to return 404, got %d body %s", recorder.Code, recorder.Body.String())
	}

	uploadID := head.URL.Query().Get("upload_id")
	metadata := filepath.Join(config.Get().System.RootDirectory, "resumable-uploads", testUploadServerUUID, uploadID+".json")
	if _, err := os.Stat(metadata); !os.IsNotExist(err) {
		t.Fatalf("expected denied metadata to be deleted, got %v", err)
	}
}

// TestResumableUploadCredentialsStayOutOfURLs enforces header auth and upload-scoped refresh tokens.
func TestResumableUploadCredentialsStayOutOfURLs(t *testing.T) {
	_, handler := newUploadTestRouter(t)
	queryCredential := signedUploadURL(t, "/upload/file?directory=/&file=world.tar", testUploadUserUUID)
	request := httptest.NewRequest(http.MethodPost, queryCredential, nil)
	request.Header.Set("Upload-Complete", "?0")
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	request.Header.Set("Upload-ID", uuid.NewString())
	request.Header.Set("Upload-Length", "11")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected query-only resumable credential to return 401, got %d body %s", recorder.Code, recorder.Body.String())
	}

	location := createResumableUpload(t, handler, "world.tar", 11)
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	scoped := resumableRequest(t, http.MethodPost, location, nil)
	query := scoped.URL.Query()
	query.Del("upload_id")
	query.Set("directory", "/")
	query.Set("file", "other.tar")
	scoped.URL.RawQuery = query.Encode()
	scoped.Header.Set("Upload-Complete", "?0")
	scoped.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	scoped.Header.Set("Upload-ID", uuid.NewString())
	scoped.Header.Set("Upload-Length", "11")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, scoped)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected upload-scoped credential not to create sessions, got %d body %s", recorder.Code, recorder.Body.String())
	}
	if parsed.Query().Get("upload_id") == "" {
		t.Fatal("expected test session identity")
	}
}

// TestResumableUploadEnforcesActiveSessionCaps bounds JSON metadata and per-user active state.
func TestResumableUploadEnforcesActiveSessionCaps(t *testing.T) {
	previous := config.Get()
	next := *previous
	next.AuthenticationToken = "upload-test-secret"
	next.Token.Token = "upload-test-secret"
	next.Api.UploadLimit = 10
	next.Api.ResumableUploads.MaxActiveSessionsPerUser = 1
	next.System.RootDirectory = t.TempDir()
	next.System.Data = filepath.Join(next.System.RootDirectory, "volumes")
	next.System.DiskCheckInterval = 150
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	_, handler := newUploadTestRouterFromCurrentConfig(t)
	_ = createResumableUpload(t, handler, "first.tar", 11)
	requestURL := signedUploadURL(t, "/upload/file?directory=/&file=second.tar", testUploadUserUUID)
	request := resumableRequest(t, http.MethodPost, requestURL, nil)
	request.Header.Set("Upload-Complete", "?0")
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	request.Header.Set("Upload-ID", uuid.NewString())
	request.Header.Set("Upload-Length", "11")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second active session to return 429, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

// TestResumableUploadCreationIsIdempotent recovers when the first 201 response never reaches the browser.
func TestResumableUploadCreationIsIdempotent(t *testing.T) {
	_, handler := newUploadTestRouter(t)
	uploadID := uuid.NewString()
	create := func() *httptest.ResponseRecorder {
		requestURL := signedUploadURL(t, "/upload/file?directory=/&file=world.tar", testUploadUserUUID)
		request := resumableRequest(t, http.MethodPost, requestURL, nil)
		request.Header.Set("Upload-Complete", "?0")
		request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
		request.Header.Set("Upload-ID", uploadID)
		request.Header.Set("Upload-Length", "11")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	first := create()
	second := create()
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("expected both creation attempts to return 201, got first=%d second=%d", first.Code, second.Code)
	}
	if first.Header().Get("Upload-ID") != uploadID || second.Header().Get("Upload-ID") != uploadID {
		t.Fatalf("expected both attempts to retain upload id %q", uploadID)
	}
	metadata, err := filepath.Glob(filepath.Join(config.Get().System.RootDirectory, "resumable-uploads", testUploadServerUUID, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 {
		t.Fatalf("expected exactly one durable session, got %d", len(metadata))
	}
}

// TestNormalizeUploadTargetEnforcesFilesystemLengthLimits rejects impossible paths before metadata creation.
func TestNormalizeUploadTargetEnforcesFilesystemLengthLimits(t *testing.T) {
	previous := config.Get()
	next := *previous
	next.Api.ResumableUploads.MaxFilenameBytes = 4
	next.Api.ResumableUploads.MaxPathBytes = 8
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	if _, err := normalizeUploadTarget("/", "12345"); err == nil {
		t.Fatal("expected oversized filename to be rejected")
	}
	if _, err := normalizeUploadTarget("/12345", "file"); err == nil {
		t.Fatal("expected oversized directory segment to be rejected")
	}
	if _, err := normalizeUploadTarget("/dir", "file"); err != nil {
		t.Fatalf("expected target at configured limit to pass, got %v", err)
	}
}

// TestResumableUploadPurgeRemovesDurableAndIndexedState covers the server-deletion lifecycle boundary.
func TestResumableUploadPurgeRemovesDurableAndIndexedState(t *testing.T) {
	server, manager, handler := newUploadTestState(t)
	location := createResumableUpload(t, handler, "world.tar", 11)
	partial := uploadChunk(t, handler, location, 0, false, []byte("hello"))
	if partial.Code != http.StatusNoContent {
		t.Fatalf("expected partial upload to return 204, got %d body %s", partial.Code, partial.Body.String())
	}

	if err := manager.Uploads().PurgeServer(server); err != nil {
		t.Fatal(err)
	}
	metadataDirectory := filepath.Join(config.Get().System.RootDirectory, "resumable-uploads", testUploadServerUUID)
	if _, err := os.Stat(metadataDirectory); !os.IsNotExist(err) {
		t.Fatalf("expected server upload metadata to be removed, got %v", err)
	}
	partials, err := filepath.Glob(filepath.Join(server.Filesystem().Path(), ".wings-upload-*.part"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("expected server upload partials to be removed, got %v", partials)
	}

	head := resumableRequest(t, http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, head)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected purged upload to return 404, got %d body %s", recorder.Code, recorder.Body.String())
	}
}
