package modpackinstall

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/filesystem"
)

// newTestFs builds a real *filesystem.Filesystem rooted in a fresh temp
// directory so clean profile tests exercise the same disk-facing code the
// installer runs against in production rather than a mock. The quota is
// unlimited so Write never fails on space, and written files are chowned to
// the test process's own uid/gid, since production's isTest shortcut lives
// in an unexported field this package cannot reach from outside.
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

// mustWrite writes content to path on fs, failing the test immediately if
// the write does not succeed.
func mustWrite(t *testing.T, fs *filesystem.Filesystem, path, content string) {
	t.Helper()

	if err := fs.Write(path, strings.NewReader(content), int64(len(content)), 0o644); err != nil {
		t.Fatalf("mustWrite %q: %v", path, err)
	}
}

// assertExists fails the test unless path is present on fs.
func assertExists(t *testing.T, fs *filesystem.Filesystem, path string) {
	t.Helper()

	if _, err := fs.UnixFS().Stat(path); err != nil {
		t.Errorf("expected %q to exist, stat failed: %v", path, err)
	}
}

// assertMissing fails the test unless path is absent from fs.
func assertMissing(t *testing.T, fs *filesystem.Filesystem, path string) {
	t.Helper()

	_, err := fs.UnixFS().Stat(path)
	if err == nil {
		t.Errorf("expected %q to be missing, but it still exists", path)
		return
	}
	if !errors.Is(err, ufs.ErrNotExist) {
		t.Errorf("expected %q to be missing, stat failed with unexpected error: %v", path, err)
	}
}

// assertContent fails the test unless path exists on fs and its content is
// exactly want.
func assertContent(t *testing.T, fs *filesystem.Filesystem, path, want string) {
	t.Helper()

	f, _, err := fs.File(path)
	if err != nil {
		t.Errorf("expected %q to exist, open failed: %v", path, err)
		return
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Errorf("expected %q to be readable, read failed: %v", path, err)
		return
	}
	if string(got) != want {
		t.Errorf("expected %q to contain %q, got %q", path, want, string(got))
	}
}

// assertSymlink fails the test unless path exists on fs as a symlink
// pointing at wantTarget. It reads the link's own target text rather than
// following it, since the target need not exist on disk.
func assertSymlink(t *testing.T, fs *filesystem.Filesystem, path, wantTarget string) {
	t.Helper()

	info, err := fs.UnixFS().Lstat(path)
	if err != nil {
		t.Errorf("expected %q to exist, lstat failed: %v", path, err)
		return
	}
	if info.Mode()&ufs.ModeSymlink == 0 {
		t.Errorf("expected %q to be a symlink, mode was %v", path, info.Mode())
		return
	}

	got, err := os.Readlink(filepath.Join(fs.Path(), path))
	if err != nil {
		t.Errorf("expected %q to be readable as a symlink, readlink failed: %v", path, err)
		return
	}
	if got != wantTarget {
		t.Errorf("expected %q to point at %q, got %q", path, wantTarget, got)
	}
}
