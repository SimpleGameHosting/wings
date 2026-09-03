package setupapply

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server/filesystem"
)

// newTestFs builds a real *filesystem.Filesystem rooted in a fresh temp
// directory so the file semantics are tested against the same disk-facing
// code the job runs against in production rather than a mock.
func newTestFs(t *testing.T) *filesystem.Filesystem {
	t.Helper()

	cfg := config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     "/server",
			DiskCheckInterval: 150,
		},
	}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(&cfg)

	fs, err := filesystem.New(filepath.Join(t.TempDir(), "server"), 0, nil)
	if err != nil {
		t.Fatalf("newTestFs: %v", err)
	}
	return fs
}

// mustWrite writes content to path on fs, failing the test on any error.
func mustWrite(t *testing.T, fs *filesystem.Filesystem, path, content string) {
	t.Helper()
	if err := fs.Write(path, strings.NewReader(content), int64(len(content)), 0o644); err != nil {
		t.Fatalf("mustWrite %q: %v", path, err)
	}
}

// mustRead returns the content at path on fs, failing the test on any error.
func mustRead(t *testing.T, fs *filesystem.Filesystem, path string) string {
	t.Helper()
	f, err := fs.UnixFS().Open(path)
	if err != nil {
		t.Fatalf("mustRead open %q: %v", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("mustRead %q: %v", path, err)
	}
	return string(b)
}
