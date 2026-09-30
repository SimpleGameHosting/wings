package modpackinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanVersionProfilePreservesConfigs(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "config/server-settings.toml", "keep")
	mustWrite(t, fs, "server.properties", "keep")
	mustWrite(t, fs, "world/level.dat", "keep")
	mustWrite(t, fs, "server.jar", "old")
	mustWrite(t, fs, "unix_args.txt", "old")
	mustWrite(t, fs, "libraries/net/x/y.jar", "old")
	mustWrite(t, fs, "forge-1.20.1-installer.jar", "old")
	mustWrite(t, fs, TempArchiveName, "crashed download")
	mustWrite(t, fs, StagingDirName+"/left/over.txt", "crashed staging")

	if err := Clean(fs, KindVersion, false); err != nil {
		t.Fatalf("clean: %v", err)
	}

	assertExists(t, fs, "config/server-settings.toml")
	assertExists(t, fs, "server.properties")
	assertExists(t, fs, "world/level.dat")
	assertMissing(t, fs, "server.jar")
	assertMissing(t, fs, "unix_args.txt")
	assertMissing(t, fs, "libraries")
	assertMissing(t, fs, "forge-1.20.1-installer.jar")
	assertMissing(t, fs, TempArchiveName)
	assertMissing(t, fs, StagingDirName)
}

// A NeoForge or Forge egg install that dies between downloading its
// installer and its own cleanup line strands installer.jar at the root,
// and finalize refuses to run with it there, so the version profile has
// to sweep the installer artifacts the legacy script deleted itself.
func TestCleanVersionProfileSweepsStrandedLoaderInstaller(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "installer.jar", "<html>404</html>")
	mustWrite(t, fs, "installer.jar.log", "stranded")
	mustWrite(t, fs, "world/level.dat", "keep")
	mustWrite(t, fs, "custom-plugin.jar", "keep")

	if err := Clean(fs, KindVersion, false); err != nil {
		t.Fatalf("clean: %v", err)
	}

	assertMissing(t, fs, "installer.jar")
	assertMissing(t, fs, "installer.jar.log")
	assertExists(t, fs, "world/level.dat")
	assertExists(t, fs, "custom-plugin.jar")
}

func TestCleanModpackProfileWipesEverything(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "config/keep.toml", "x")
	mustWrite(t, fs, "world/level.dat", "x")
	mustWrite(t, fs, TempArchiveName, "x")

	if err := Clean(fs, KindModpack, false); err != nil {
		t.Fatalf("clean: %v", err)
	}

	entries, err := fs.ReadDir("/")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("root not empty after modpack clean: %d entries", len(entries))
	}
}

// A version install the panel asked to wipe must leave nothing behind at
// the root, worlds and configs included, since that is exactly what the
// customer confirmed losing in the panel's "Wipe and install" option. It
// must also only ever unlink a symlink rather than follow it, so nothing
// outside the server root is touched.
func TestCleanVersionWipeRemovesEverything(t *testing.T) {
	fs := newTestFs(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(fs.Path(), "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	mustWrite(t, fs, "world/level.dat", "x")
	mustWrite(t, fs, "config/server-settings.toml", "x")
	mustWrite(t, fs, "server.properties", "x")
	mustWrite(t, fs, "plugins/custom-plugin.jar", "x")
	mustWrite(t, fs, ".hidden", "x")
	mustWrite(t, fs, "unix_args.txt", "old")
	mustWrite(t, fs, "libraries/net/minecraftforge/forge/26.2-62.0.1/unix_args.txt", "old")
	mustWrite(t, fs, TempArchiveName, "x")

	if err := Clean(fs, KindVersion, true); err != nil {
		t.Fatalf("clean: %v", err)
	}

	entries, err := fs.ReadDir("/")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("root not empty after version wipe: %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("a file outside the server root did not survive the wipe: %v", err)
	}
}
