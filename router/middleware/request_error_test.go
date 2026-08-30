package middleware

import (
	"net/http"
	"os"
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
