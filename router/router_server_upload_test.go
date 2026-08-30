package router

import (
	"bytes"
	"encoding/json"
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

	return newUploadTestRouterFromCurrentConfig(t)
}

// newUploadTestRouterFromCurrentConfig recreates Wings state while retaining durable upload files.
func newUploadTestRouterFromCurrentConfig(t *testing.T) (*wserver.Server, http.Handler) {
	t.Helper()

	client := backupTestRemoteClient{}
	manager := wserver.NewEmptyManager(client)
	server, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"` + testUploadServerUUID + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Add(server)
	t.Cleanup(server.CtxCancel)

	return server, Configure(manager, client)
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
func signedUploadURL(t *testing.T, requestURL string, userUUID string) string {
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

// createResumableUpload starts an upload session and returns the signed session URL emitted by Wings.
func createResumableUpload(t *testing.T, handler http.Handler, target string, size int64) string {
	t.Helper()

	requestURL := signedUploadURL(t, "/upload/file?directory=/&file="+url.QueryEscape(target), testUploadUserUUID)
	request := httptest.NewRequest(http.MethodPost, requestURL, nil)
	request.Header.Set("Upload-Complete", "?0")
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
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

	return location
}

// uploadChunk sends one browser-style resumable PATCH request to Wings.
func uploadChunk(handler http.Handler, location string, offset int64, complete bool, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPatch, location, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	completeHeader := "?0"
	if complete {
		completeHeader = "?1"
	}
	request.Header.Set("Upload-Complete", completeHeader)
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
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

	interrupted := httptest.NewRequest(http.MethodPatch, location, &interruptedUploadReader{payload: []byte("hello ")})
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

	head := httptest.NewRequest(http.MethodHead, location, nil)
	head.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	headRecorder := httptest.NewRecorder()
	handler.ServeHTTP(headRecorder, head)
	if headRecorder.Code != http.StatusNoContent || headRecorder.Header().Get("Upload-Offset") != "6" {
		t.Fatalf("expected resumable HEAD offset 6, got %d offset %q", headRecorder.Code, headRecorder.Header().Get("Upload-Offset"))
	}

	activityBefore := uploadActivityCount(t)
	last := uploadChunk(handler, location, 6, true, []byte("world"))
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

	recorder := uploadChunk(handler, location, 4, false, []byte("world"))
	if recorder.Code != http.StatusConflict || recorder.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("expected offset conflict at zero, got %d offset %q body %s", recorder.Code, recorder.Header().Get("Upload-Offset"), recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.Filesystem().Path(), "world.tar")); !os.IsNotExist(err) {
		t.Fatalf("expected destination not to be created after an offset conflict, got %v", err)
	}

	recorder = uploadChunk(handler, location, 0, true, []byte("hello wurld"))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected checksum mismatch to return 422, got %d body %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.Filesystem().Path(), "world.tar")); !os.IsNotExist(err) {
		t.Fatalf("expected corrupted content not to be published, got %v", err)
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
	otherUserLocation := signedUploadURL(t, parsed.String(), "6b44ba82-0000-4000-8000-000000000000")
	request := httptest.NewRequest(http.MethodHead, otherUserLocation, nil)
	request.Header.Set("Upload-Fingerprint", testUploadFingerprint)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected another user to receive 404, got %d body %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodHead, location, nil)
	request.Header.Set("Upload-Fingerprint", "7f44858425cc03d3e797f2707e0f58c3d641f5f6ec7b777109918a7a1f5e4b13")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected a mismatched fingerprint to receive 404, got %d body %s", recorder.Code, recorder.Body.String())
	}
}
