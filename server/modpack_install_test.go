package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/modpackinstall"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/remote"
)

// ensureModpackInstallSlotDefaults pins the node-wide install cap to its
// documented default (3) regardless of what an earlier test in this package
// left in the global configuration. Several tests elsewhere in this package
// call config.Set with a bare Configuration literal, which replaces the
// whole global struct without reapplying the "default" struct tags, so a
// test that depends on config.Get() still holding Task 7's defaults cannot
// simply trust ambient state left by whatever ran before it.
func ensureModpackInstallSlotDefaults(t *testing.T) {
	t.Helper()
	config.Update(func(c *config.Configuration) {
		c.System.ModpackInstall.MaxConcurrent = 3
	})
}

// TestModpackInstallSlotCap locks down the node-wide concurrency ceiling: a
// reservation is granted below the cap, refused once it is reached, and a
// released slot becomes available again through an idempotent release func.
func TestModpackInstallSlotCap(t *testing.T) {
	ensureModpackInstallSlotDefaults(t)
	m := NewEmptyManager(nil)
	// Default cap is 3.
	var releases []func()
	for i := 0; i < 3; i++ {
		rel, ok := m.TryReserveModpackInstallSlot()
		if !ok {
			t.Fatalf("slot %d refused below cap", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := m.TryReserveModpackInstallSlot(); ok {
		t.Fatal("4th slot must be refused")
	}
	releases[0]()
	releases[0]() // double release must be safe
	if _, ok := m.TryReserveModpackInstallSlot(); !ok {
		t.Fatal("slot must free after release")
	}
}

// TestModpackInstallSlotConcurrency hammers the reservation from 32 parallel
// goroutines and checks the mutex-guarded counter never grants more than the
// configured cap, the property a bare unsynchronized counter would violate
// under -race.
func TestModpackInstallSlotConcurrency(t *testing.T) {
	ensureModpackInstallSlotDefaults(t)
	m := NewEmptyManager(nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := m.TryReserveModpackInstallSlot(); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 3 {
		t.Errorf("granted=%d want exactly 3", granted)
	}
}

// modpackInstallTestClient records the context a finished install reported
// its outcome with, observed at call time.
type modpackInstallTestClient struct {
	remote.Client

	reports chan fencedReportContext
}

func (c modpackInstallTestClient) SendModpackInstallResult(ctx context.Context, _ string, _ remote.ModpackInstallResultRequest) error {
	c.reports <- observeFencedReportContext(ctx)
	return nil
}

// newModpackInstallServer builds a server whose panel client only records
// the install result callback, which is all the finisher needs.
func newModpackInstallServer(t *testing.T) (*Server, modpackInstallTestClient) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	settings := json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, uuid.NewString()))
	client := modpackInstallTestClient{reports: make(chan fencedReportContext, 1)}
	s, err := NewEmptyManager(client).InitServer(setupApplyServerConfiguration(settings))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	return s, client
}

// TestFinishModpackInstallReportsTheResultWithABoundedContext checks the
// panel callback carries a deadline of its own instead of the attempt's
// context, which on the timeout path is already expired when the report is
// sent.
func TestFinishModpackInstallReportsTheResultWithABoundedContext(t *testing.T) {
	s, client := newModpackInstallServer(t)

	s.finishModpackInstall(modpackinstall.Request{InstallID: uuid.NewString()}, nil, time.Now(), func() {})

	report := <-client.reports
	if !report.bounded {
		t.Fatal("the result report must be sent with a bounded context")
	}
	if report.err != nil {
		t.Fatalf("the result report context was already done: %v", report.err)
	}
}

// modpackInstallPipelineClient answers the configuration sync the pipeline
// performs and captures the terminal result, so a whole install attempt
// can run against a real temp filesystem without a panel behind it.
type modpackInstallPipelineClient struct {
	remote.Client

	settings json.RawMessage
	results  chan remote.ModpackInstallResultRequest
}

func (c modpackInstallPipelineClient) GetServerConfiguration(_ context.Context, _ string) (remote.ServerConfigurationResponse, error) {
	return setupApplyServerConfiguration(c.settings), nil
}

func (c modpackInstallPipelineClient) SendModpackInstallResult(_ context.Context, _ string, data remote.ModpackInstallResultRequest) error {
	c.results <- data
	return nil
}

// runJarVersionInstall runs one complete jar-format version install,
// optionally wiping, against a server whose root already holds a world, a
// config, and the previous version's files, and returns the server and
// the result the panel would receive.
func runJarVersionInstall(t *testing.T, wipe bool) (*Server, remote.ModpackInstallResultRequest) {
	t.Helper()

	previous := config.Get()
	next := *previous
	next.System.Data = t.TempDir()
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	next.System.ModpackInstall.TimeoutMinutes = 1
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	settings := json.RawMessage(fmt.Sprintf(`{"uuid":%q}`, uuid.NewString()))
	client := modpackInstallPipelineClient{settings: settings, results: make(chan remote.ModpackInstallResultRequest, 1)}
	s, err := NewEmptyManager(client).InitServer(setupApplyServerConfiguration(settings))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	s.Environment = &setupApplyTestEnvironment{
		started: make(chan struct{}, 1),
		state:   environment.ProcessOfflineState,
		config:  environment.NewConfiguration(environment.Settings{}, nil),
	}

	for name, content := range map[string]string{
		"world/level.dat":   "world",
		"server.properties": "motd=keep",
		"server.jar":        "old version",
		"unix_args.txt":     "@libraries/net/minecraftforge/forge/26.2/unix_args.txt",
	} {
		if err := s.Filesystem().Write(name, strings.NewReader(content), int64(len(content)), 0o644); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	jar := []byte("new version")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(jar)))
		_, _ = w.Write(jar)
	}))
	t.Cleanup(cdn.Close)

	req := modpackinstall.Request{
		InstallID:     uuid.NewString(),
		Kind:          modpackinstall.KindVersion,
		DownloadURL:   cdn.URL + "/paper/26.3.jar",
		ArchiveFormat: modpackinstall.FormatJar,
		VersionType:   modpackinstall.VersionPaper,
		Wipe:          wipe,
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if repeat, err := s.AdmitModpackInstall(req.InstallID); err != nil || repeat {
		t.Fatalf("admit: repeat=%v err=%v", repeat, err)
	}

	s.RunModpackInstall(req, func() {})

	return s, <-client.results
}

// readServerFile returns a file's content from the server root, or fails
// the test when it cannot be read.
func readServerFile(t *testing.T, s *Server, name string) string {
	t.Helper()

	f, _, err := s.Filesystem().File(name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}
	defer f.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, f); err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return b.String()
}

// serverFileExists reports whether name is present at the server root.
func serverFileExists(t *testing.T, s *Server, name string) bool {
	t.Helper()

	_, err := s.Filesystem().UnixFS().Lstat(name)
	if err == nil {
		return true
	}
	if !errors.Is(err, ufs.ErrNotExist) {
		t.Fatalf("stat %q: %v", name, err)
	}
	return false
}

// TestRunModpackInstallVersionWithoutWipeKeepsTheWorld pins the default: a
// version install replaces the loader but never touches worlds or configs.
func TestRunModpackInstallVersionWithoutWipeKeepsTheWorld(t *testing.T) {
	s, result := runJarVersionInstall(t, false)

	if !result.Successful {
		t.Fatalf("install failed: %+v", result)
	}
	if got := readServerFile(t, s, "world/level.dat"); got != "world" {
		t.Fatalf("world/level.dat = %q, want it untouched", got)
	}
	if got := readServerFile(t, s, "server.properties"); got != "motd=keep" {
		t.Fatalf("server.properties = %q, want it untouched", got)
	}
	if got := readServerFile(t, s, "server.jar"); got != "new version" {
		t.Fatalf("server.jar = %q, want the new version", got)
	}
	if serverFileExists(t, s, "unix_args.txt") {
		t.Fatal("the previous loader's unix_args.txt survived the install")
	}
}

// TestRunModpackInstallVersionWithWipeRemovesEverythingElse proves a wipe
// requested by the panel reaches the clean stage: afterwards the root holds
// only what the install itself produced.
func TestRunModpackInstallVersionWithWipeRemovesEverythingElse(t *testing.T) {
	s, result := runJarVersionInstall(t, true)

	if !result.Successful {
		t.Fatalf("install failed: %+v", result)
	}
	for _, name := range []string{"world", "server.properties", "unix_args.txt"} {
		if serverFileExists(t, s, name) {
			t.Errorf("%q survived a wipe", name)
		}
	}
	if got := readServerFile(t, s, "server.jar"); got != "new version" {
		t.Fatalf("server.jar = %q, want the new version", got)
	}

	entries, err := s.Filesystem().ReadDir("/")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "eula.txt,server.jar" {
		t.Fatalf("root after wipe = %v, want only eula.txt and server.jar", names)
	}
}
