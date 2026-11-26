package router

import (
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	wserver "github.com/pterodactyl/wings/server"
)

func writeCompressibleFile(t *testing.T, s *wserver.Server, name string, content string) {
	t.Helper()
	if err := s.Filesystem().Write(name, strings.NewReader(content), int64(len(content)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPostServerCompressFilesReturnsAcceptedAndPublishesEvent(t *testing.T) {
	c, recorder, s := newFilesContext(t, "compress", `{"root":"/plugins/","files":["hello.txt"]}`)
	writeCompressibleFile(t, s, "plugins/hello.txt", "hello world")
	completions := make(chan []byte, 8)
	s.Events().On(completions)
	defer s.Events().Off(completions)

	postServerCompressFiles(c)

	if c.Writer.Status() != http.StatusAccepted {
		t.Fatalf("expected async compression to return 202, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
	var accepted struct {
		Name string `json:"name"`
	}
	decodeJSONBody(t, recorder, &accepted)
	if !regexp.MustCompile(`^archive-.*\.tar\.gz$`).MatchString(accepted.Name) {
		t.Fatalf("unexpected archive name %q", accepted.Name)
	}

	status := awaitCompletionEvent[wserver.FileCompressStatus](t, completions, wserver.FileCompressCompletedEvent)
	if status.Name != accepted.Name || status.Directory != "/plugins/" || !status.Successful || status.Error != "" {
		t.Fatalf("unexpected compress completion payload: %+v", status)
	}

	archive := filepath.Join(s.Filesystem().Path(), "plugins", accepted.Name)
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("expected the archive to exist: %v", err)
	}
	if _, err := os.Stat(archive + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("expected no partial archive to remain, got %v", err)
	}
}

func TestPostServerCompressFilesRejectsDuplicate(t *testing.T) {
	c, recorder, s := newFilesContext(t, "compress", `{"root":"/","files":["hello.txt"]}`)
	writeCompressibleFile(t, s, "hello.txt", "hello world")

	_, release, ok := s.Filesystem().ReserveArchive("/", []string{"hello.txt"})
	if !ok {
		t.Fatal("expected the test to be able to reserve the set first")
	}
	defer release()

	postServerCompressFiles(c)

	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected duplicate archiving to return 409, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, recorder, &body)
	if body.Error != "These files are already being archived, wait for it to finish." {
		t.Fatalf("unexpected error %q", body.Error)
	}
}

func TestPostServerCompressFilesPublishesFailureEvent(t *testing.T) {
	c, recorder, s := newFilesContext(t, "compress", `{"root":"/","files":["noise.bin"]}`)
	noise := make([]byte, 4096)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	writeCompressibleFile(t, s, "noise.bin", string(noise))
	s.Filesystem().SetDiskLimit(4200)
	completions := make(chan []byte, 8)
	s.Events().On(completions)
	defer s.Events().Off(completions)

	postServerCompressFiles(c)

	if c.Writer.Status() != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
	status := awaitCompletionEvent[wserver.FileCompressStatus](t, completions, wserver.FileCompressCompletedEvent)
	if status.Successful || !strings.Contains(status.Error, "disk space") {
		t.Fatalf("expected a disk space failure, got %+v", status)
	}

	entries, err := os.ReadDir(s.Filesystem().Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "archive-") {
			t.Fatalf("expected no archive or partial to remain, found %s", entry.Name())
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, release, ok := s.Filesystem().ReserveArchive("/", []string{"noise.bin"}); ok {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the archive reservation to be released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPostServerCompressFilesKeepsSynchronousChecks(t *testing.T) {
	c, recorder, _ := newFilesContext(t, "compress", `{"root":"/","files":[]}`)
	postServerCompressFiles(c)
	if c.Writer.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for no files, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}

	c, recorder, s := newFilesContext(t, "compress", `{"root":"/","files":["hello.txt"]}`)
	writeCompressibleFile(t, s, "hello.txt", strings.Repeat("hello world ", 100))
	s.Filesystem().SetDiskLimit(1)
	if _, err := s.Filesystem().DiskUsage(false); err != nil {
		t.Fatal(err)
	}
	postServerCompressFiles(c)
	if c.Writer.Status() != http.StatusConflict {
		t.Fatalf("expected 409 for no space, got %d body %s", c.Writer.Status(), recorder.Body.String())
	}
}
