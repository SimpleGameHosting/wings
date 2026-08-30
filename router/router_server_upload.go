package router

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
)

const (
	uploadCompleteHeader    = "Upload-Complete"
	uploadFingerprintHeader = "Upload-Fingerprint"
	uploadIDHeader          = "Upload-ID"
	uploadLengthHeader      = "Upload-Length"
	uploadOffsetHeader      = "Upload-Offset"
)

var uploadFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// authenticateServerUpload validates a signed Panel upload URL and resolves its server.
func authenticateServerUpload(c *gin.Context, unique bool) (*server.Server, *tokens.UploadPayload, bool) {
	manager := middleware.ExtractManager(c)
	payload := &tokens.UploadPayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), payload); err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, nil, false
	}

	resolved, ok := manager.Get(payload.ServerUuid)
	if !ok || payload.Denylisted() || !payload.HasScope(tokens.FileUpload) || (unique && !payload.IsUniqueRequest()) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return nil, nil, false
	}

	return resolved, payload, true
}

// postServerCreateResumableUpload creates durable state for a browser-managed chunked upload.
func postServerCreateResumableUpload(c *gin.Context) {
	resolved, payload, ok := authenticateServerUpload(c, true)
	if !ok {
		return
	}
	if c.GetHeader(uploadCompleteHeader) != "?0" {
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Complete must be ?0 when creating an upload.")
		return
	}

	length, ok := parseNonNegativeUploadHeader(c, uploadLengthHeader)
	if !ok {
		return
	}
	if !uploadFitsNodeLimit(length) {
		abortUploadRequest(c, http.StatusRequestEntityTooLarge, "The file exceeds this node's maximum upload size.")
		return
	}
	fingerprint := c.GetHeader(uploadFingerprintHeader)
	if !uploadFingerprintPattern.MatchString(fingerprint) {
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Fingerprint must be a lowercase SHA-256 value.")
		return
	}

	target, err := normalizeUploadTarget(c.Query("directory"), c.Query("file"))
	if err != nil {
		abortUploadRequest(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolved.Filesystem().IsIgnored(target); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	session, err := middleware.ExtractManager(c).Uploads().Create(resolved, payload.UserUuid, target, fingerprint, length)
	if err != nil {
		captureUploadError(c, err)
		return
	}

	location := *c.Request.URL
	query := location.Query()
	query.Del("directory")
	query.Del("file")
	query.Set("upload_id", session.ID)
	location.RawQuery = query.Encode()
	setUploadResponseHeaders(c, session)
	c.Header("Location", relativeUploadLocation(&location))
	c.Status(http.StatusCreated)
}

// headServerUploadFile reports the durable offset used to resume after a request interruption.
func headServerUploadFile(c *gin.Context) {
	resolved, payload, ok := authenticateServerUpload(c, false)
	if !ok {
		return
	}

	session, ok := resolveUploadSession(c, resolved, payload)
	if !ok {
		return
	}
	setUploadResponseHeaders(c, session)
	c.Status(http.StatusNoContent)
}

// patchServerUploadFile streams one chunk and atomically publishes the destination on completion.
func patchServerUploadFile(c *gin.Context) {
	resolved, payload, ok := authenticateServerUpload(c, false)
	if !ok {
		return
	}

	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/offset+octet-stream" {
		abortUploadRequest(c, http.StatusUnsupportedMediaType, "Content-Type must be application/offset+octet-stream.")
		return
	}
	offset, ok := parseNonNegativeUploadHeader(c, uploadOffsetHeader)
	if !ok {
		return
	}
	complete, ok := parseUploadComplete(c)
	if !ok {
		return
	}
	fingerprint := c.GetHeader(uploadFingerprintHeader)
	if !uploadFingerprintPattern.MatchString(fingerprint) {
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Fingerprint must be a lowercase SHA-256 value.")
		return
	}
	sessionBeforeWrite, err := middleware.ExtractManager(c).Uploads().Status(
		resolved,
		payload.UserUuid,
		c.Query("upload_id"),
		fingerprint,
	)
	if err != nil {
		captureUploadError(c, err)
		return
	}
	if !uploadFitsNodeLimit(sessionBeforeWrite.Size) {
		abortUploadRequest(c, http.StatusRequestEntityTooLarge, "The file exceeds this node's maximum upload size.")
		return
	}

	session, completedNow, err := middleware.ExtractManager(c).Uploads().WriteChunk(
		c.Request.Context(),
		resolved,
		payload.UserUuid,
		c.Query("upload_id"),
		fingerprint,
		offset,
		c.Request.ContentLength,
		complete,
		c.Request.Body,
	)
	if err != nil {
		captureUploadError(c, err)
		return
	}

	if completedNow {
		directory := filepath.Dir(session.Target)
		if directory == "." {
			directory = "/"
		}
		resolved.SaveActivity(resolved.NewRequestActivity(payload.UserUuid, c.ClientIP()), server.ActivityFileUploaded, models.ActivityMeta{
			"file":      filepath.Base(session.Target),
			"directory": directory,
		})
	}

	setUploadResponseHeaders(c, session)
	c.Status(http.StatusNoContent)
}

// deleteServerUploadFile cancels a session without modifying an existing destination file.
func deleteServerUploadFile(c *gin.Context) {
	resolved, payload, ok := authenticateServerUpload(c, false)
	if !ok {
		return
	}
	fingerprint := c.GetHeader(uploadFingerprintHeader)
	if !uploadFingerprintPattern.MatchString(fingerprint) {
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Fingerprint must be a lowercase SHA-256 value.")
		return
	}

	err := middleware.ExtractManager(c).Uploads().Cancel(
		resolved,
		payload.UserUuid,
		c.Query("upload_id"),
		fingerprint,
	)
	if err != nil {
		captureUploadError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// resolveUploadSession loads an authorized upload after validating its request fingerprint.
func resolveUploadSession(c *gin.Context, resolved *server.Server, payload *tokens.UploadPayload) (server.UploadSession, bool) {
	fingerprint := c.GetHeader(uploadFingerprintHeader)
	if !uploadFingerprintPattern.MatchString(fingerprint) {
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Fingerprint must be a lowercase SHA-256 value.")
		return server.UploadSession{}, false
	}

	session, err := middleware.ExtractManager(c).Uploads().Status(
		resolved,
		payload.UserUuid,
		c.Query("upload_id"),
		fingerprint,
	)
	if err != nil {
		captureUploadError(c, err)
		return server.UploadSession{}, false
	}
	return session, true
}

// setUploadResponseHeaders exposes the canonical session state to browser retry logic.
func setUploadResponseHeaders(c *gin.Context, session server.UploadSession) {
	c.Header("Cache-Control", "no-store")
	c.Header(uploadIDHeader, session.ID)
	c.Header(uploadLengthHeader, strconv.FormatInt(session.Size, 10))
	c.Header(uploadOffsetHeader, strconv.FormatInt(session.Offset, 10))
	if session.Complete() {
		c.Header(uploadCompleteHeader, "?1")
	} else {
		c.Header(uploadCompleteHeader, "?0")
	}
}

// parseNonNegativeUploadHeader parses a required decimal request header.
func parseNonNegativeUploadHeader(c *gin.Context, name string) (int64, bool) {
	value := c.GetHeader(name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		abortUploadRequest(c, http.StatusBadRequest, name+" must be a non-negative integer.")
		return 0, false
	}
	return parsed, true
}

// parseUploadComplete validates the structured boolean used by resumable upload requests.
func parseUploadComplete(c *gin.Context) (bool, bool) {
	switch c.GetHeader(uploadCompleteHeader) {
	case "?0":
		return false, true
	case "?1":
		return true, true
	default:
		abortUploadRequest(c, http.StatusBadRequest, "Upload-Complete must be ?0 or ?1.")
		return false, false
	}
}

// normalizeUploadTarget converts Panel directory and filename fields into one safe relative path.
func normalizeUploadTarget(directory, filename string) (string, error) {
	if filename == "" || filename == "." || filename == ".." || filepath.Base(filename) != filename {
		return "", fmt.Errorf("file must contain a single filename")
	}
	if strings.HasPrefix(filename, ".wings-upload-") {
		return "", fmt.Errorf("file uses a reserved upload filename")
	}

	cleanDirectory := filepath.Clean(strings.TrimLeft(directory, "/"))
	if cleanDirectory == ".." || strings.HasPrefix(cleanDirectory, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("directory resolves outside the server root")
	}
	if cleanDirectory == "." {
		return filename, nil
	}
	return filepath.Join(cleanDirectory, filename), nil
}

// uploadFitsNodeLimit checks the same configured byte ceiling used by legacy multipart uploads.
func uploadFitsNodeLimit(size int64) bool {
	limit := config.Get().Api.UploadLimit * 1024 * 1024
	return size <= limit
}

// captureUploadError maps resumable protocol errors while preserving Wings filesystem responses.
func captureUploadError(c *gin.Context, err error) {
	var offsetError *server.UploadOffsetError
	switch {
	case errors.As(err, &offsetError):
		c.Header(uploadOffsetHeader, strconv.FormatInt(offsetError.Expected, 10))
		abortUploadRequest(c, http.StatusConflict, "The upload offset does not match the server offset.")
	case errors.Is(err, server.ErrUploadConflict):
		abortUploadRequest(c, http.StatusConflict, "Another active upload already targets this file.")
	case errors.Is(err, server.ErrUploadIncomplete):
		abortUploadRequest(c, http.StatusBadRequest, "The upload cannot complete before all bytes arrive.")
	case errors.Is(err, server.ErrUploadChecksumMismatch):
		abortUploadRequest(c, http.StatusUnprocessableEntity, "The uploaded content does not match its SHA-256 fingerprint.")
	case errors.Is(err, server.ErrUploadTooLarge):
		abortUploadRequest(c, http.StatusRequestEntityTooLarge, "The chunk exceeds the declared upload length.")
	case errors.Is(err, server.ErrUploadNotFound), errors.Is(err, server.ErrUploadFingerprintMismatch):
		abortUploadRequest(c, http.StatusNotFound, "The requested upload session was not found.")
	default:
		middleware.CaptureAndAbort(c, err)
	}
}

// abortUploadRequest returns a concise protocol error without routing expected client mistakes through error reporting.
func abortUploadRequest(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}

// relativeUploadLocation strips scheme and host data so signed upload URLs remain node-relative.
func relativeUploadLocation(location *url.URL) string {
	return location.RequestURI()
}
