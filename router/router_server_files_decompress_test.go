package router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/remote"
	wserver "github.com/pterodactyl/wings/server"
)

// newDecompressContext creates a real server filesystem and a Gin request
// context matching the panel's decompress request path.
func newDecompressContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder, *wserver.Server) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	manager := wserver.NewEmptyManager(backupTestRemoteClient{})
	s, err := manager.InitServer(remote.ServerConfigurationResponse{
		Settings: json.RawMessage(`{"uuid":"8d2b3f6a-0000-4000-8000-000000000000"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/servers/8d2b3f6a-0000-4000-8000-000000000000/files/decompress", strings.NewReader(body))
	c.Request = request
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, recorder, s
}

// writeTestTarball writes a small tar.gz archive holding unpacked/hello.txt
// into the server's filesystem under the given name.
func writeTestTarball(t *testing.T, s *wserver.Server, name string) {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("hello world")
	if err := tw.WriteHeader(&tar.Header{Name: "unpacked/hello.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Filesystem().Write(name, bytes.NewReader(buf.Bytes()), int64(buf.Len()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPostServerDecompressFilesReturnsAcceptedAndExtractsInBackground covers
// pterodactyl/panel#2878: large archives outlive proxy timeouts, so the
// endpoint must accept the request immediately and extract in the background.
func TestPostServerDecompressFilesReturnsAcceptedAndExtractsInBackground(t *testing.T) {
	c, recorder, s := newDecompressContext(t, `{"root":"/","file":"bundle.tar.gz"}`)
	writeTestTarball(t, s, "bundle.tar.gz")

	postServerDecompressFiles(c)

	if c.Writer.Status() != http.StatusAccepted {
		t.Fatalf("expected async decompression to return 202, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	extracted := filepath.Join(s.Filesystem().Path(), "unpacked/hello.txt")
	for {
		if contents, err := os.ReadFile(extracted); err == nil {
			if string(contents) != "hello world" {
				t.Fatalf("expected extracted contents to match, got %q", contents)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the background extraction to finish")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The worker must release its single-flight reservation once it finishes;
	// otherwise every later decompression of this archive would be rejected
	// until the daemon restarts...
	releaseDeadline := time.Now().Add(5 * time.Second)
	for {
		if release, ok := s.Filesystem().TryStartDecompression("/", "bundle.tar.gz"); ok {
			release()
			return
		}
		if time.Now().After(releaseDeadline) {
			t.Fatal("timed out waiting for the decompression reservation to be released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPostServerDecompressFilesRejectsUnknownFormatSynchronously keeps the
// immediate, accurate 400 for archives Wings cannot understand.
func TestPostServerDecompressFilesRejectsUnknownFormatSynchronously(t *testing.T) {
	c, recorder, s := newDecompressContext(t, `{"root":"/","file":"noise.bin"}`)
	noise := "definitely not an archive"
	if err := s.Filesystem().Write("noise.bin", strings.NewReader(noise), int64(len(noise)), 0o644); err != nil {
		t.Fatal(err)
	}

	postServerDecompressFiles(c)

	if c.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("expected unknown archive format to return 400, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}

// TestPostServerDecompressFilesRejectsConcurrentDuplicate ensures a second
// request for an archive that is already being decompressed is refused
// instead of spawning a competing extraction into the same tree.
func TestPostServerDecompressFilesRejectsConcurrentDuplicate(t *testing.T) {
	c, recorder, s := newDecompressContext(t, `{"root":"/","file":"bundle.tar.gz"}`)
	writeTestTarball(t, s, "bundle.tar.gz")

	release, ok := s.Filesystem().TryStartDecompression("/", "bundle.tar.gz")
	if !ok {
		t.Fatal("expected the test to be able to reserve the archive first")
	}
	defer release()

	postServerDecompressFiles(c)

	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected duplicate decompression to return 409, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}

// TestDecompressRecoveryGuardContainsPanics ensures a panic from the archive
// parser inside the background goroutine is converted into a console message
// and an error log instead of crashing the whole daemon: the archive bytes
// driving the parser are tenant controlled.
func TestDecompressRecoveryGuardContainsPanics(t *testing.T) {
	_, _, s := newDecompressContext(t, `{}`)

	consoleMessages := make(chan []byte, 4)
	s.Events().On(consoleMessages)
	defer s.Events().Off(consoleMessages)

	decompressRecoveryGuard(s, "boom.tar.gz", func() { panic("simulated archive parser panic") })

	select {
	case raw := <-consoleMessages:
		event := events.MustDecode(raw)
		if event.Topic != wserver.ConsoleOutputEvent {
			t.Fatalf("expected a console output event, got topic %q", event.Topic)
		}
		message, _ := event.Data.(string)
		if !strings.Contains(message, "failed") {
			t.Fatalf("expected the console message to report the failure, got %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a console message after the panic was recovered")
	}
}

// TestPostServerDecompressFilesRejectsMissingFileSynchronously ensures a
// request for an archive that does not exist aborts through the error
// middleware and never reaches the background extraction path.
func TestPostServerDecompressFilesRejectsMissingFileSynchronously(t *testing.T) {
	c, _, _ := newDecompressContext(t, `{"root":"/","file":"missing.tar.gz"}`)

	postServerDecompressFiles(c)

	if c.Writer.Status() == http.StatusAccepted {
		t.Fatalf("expected a missing archive to be rejected, got 202")
	}
	if !c.IsAborted() || len(c.Errors) == 0 {
		t.Fatal("expected the request to abort through the error middleware")
	}
}
