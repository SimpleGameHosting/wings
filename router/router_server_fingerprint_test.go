package router

import (
	"context"
	"encoding/hex"
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
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
)

// newFingerprintContext creates a real server filesystem and a Gin request
// context matching the Panel's fingerprint request path.
func newFingerprintContext(t *testing.T, requestContext context.Context) (*gin.Context, *httptest.ResponseRecorder, *wserver.Server) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()

	// Chown written files to the test process itself so the filesystem layer
	// does not need root (CI runners are unprivileged).
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() {
		config.Set(previous)
	})

	client := backupTestRemoteClient{}
	manager := wserver.NewEmptyManager(client)
	s, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"8d2b3f6a-0000-4000-8000-000000000000"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/servers/8d2b3f6a-0000-4000-8000-000000000000/fingerprint", strings.NewReader(`{"ignore":""}`))
	c.Request = request.WithContext(requestContext)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("logger", log.WithField("test", t.Name()))

	return c, recorder, s
}

// TestPostServerFingerprintHonoursRequestDeadline ensures an expired HTTP
// request stops its filesystem walk and returns a timeout.
func TestPostServerFingerprintHonoursRequestDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	c, recorder, _ := newFingerprintContext(t, ctx)

	postServerFingerprint(c)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected cancelled fingerprint request to return 504, got %d body %s", recorder.Code, recorder.Body.String())
	}
}

// TestPostServerFingerprintReturnsWellFormedResult verifies the successful
// handler response consumed by the Panel.
func TestPostServerFingerprintReturnsWellFormedResult(t *testing.T) {
	c, recorder, s := newFingerprintContext(t, context.Background())
	contents := strings.NewReader("motd=hello\n")
	if err := s.Filesystem().Write("server.properties", contents, contents.Size(), 0o644); err != nil {
		t.Fatal(err)
	}

	postServerFingerprint(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected fingerprint request to return 200, got %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Fingerprint string `json:"fingerprint"`
		Files       int    `json:"files"`
		DurationMs  int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(result.Fingerprint); err != nil || len(result.Fingerprint) != 64 {
		t.Fatalf("expected a 64-character hexadecimal fingerprint, got %q", result.Fingerprint)
	}
	if result.Files != 1 {
		t.Fatalf("expected one fingerprinted file, got %d", result.Files)
	}
	if result.DurationMs < 0 {
		t.Fatalf("expected non-negative duration, got %d", result.DurationMs)
	}
}

// TestFingerprintRouteReturnsNotFoundForUnknownServer verifies the registered
// endpoint uses Wings' standard server-existence middleware.
func TestFingerprintRouteReturnsNotFoundForUnknownServer(t *testing.T) {
	client := backupTestRemoteClient{}
	router := Configure(wserver.NewEmptyManager(client), client)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/servers/missing/fingerprint", strings.NewReader(`{"ignore":""}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected unknown fingerprint server to return 404, got %d body %s", recorder.Code, recorder.Body.String())
	}
}
