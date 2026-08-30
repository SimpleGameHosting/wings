package middleware

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestRequestErrorMapsRawNotExistToNotFound verifies that low-level filesystem
// errors keep their HTTP semantics when they reach the router middleware.
func TestRequestErrorMapsRawNotExistToNotFound(t *testing.T) {
	errorFromOpenat2 := &os.PathError{Op: "openat2", Path: "ops.json", Err: os.ErrNotExist}

	status, message := NewError(errorFromOpenat2).asFilesystemError()

	if status != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, status)
	}
	if message != "The requested resources was not found on the system." {
		t.Fatalf("unexpected response message: %q", message)
	}
}

// TestRedactRequestURLRemovesSignedToken ensures upload failures never expose reusable JWTs in logs.
func TestRedactRequestURLRemovesSignedToken(t *testing.T) {
	requestURL, err := url.Parse("/upload/file?token=signed-secret&upload_id=123")
	if err != nil {
		t.Fatal(err)
	}

	redacted := redactRequestURL(requestURL)
	if strings.Contains(redacted, "signed-secret") {
		t.Fatalf("expected signed token to be redacted, got %q", redacted)
	}
	if !strings.Contains(redacted, "upload_id=123") || !strings.Contains(redacted, "token=%5Bredacted%5D") {
		t.Fatalf("expected non-secret query data and redaction marker, got %q", redacted)
	}
}
