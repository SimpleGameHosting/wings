package modpackinstall

import (
	"testing"
)

func TestCleanVersionProfilePreservesConfigs(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "config/server-settings.toml", "keep")
	mustWrite(t, fs, "server.properties", "keep")
	mustWrite(t, fs, "server.jar", "old")
	mustWrite(t, fs, "unix_args.txt", "old")
	mustWrite(t, fs, "libraries/net/x/y.jar", "old")
	mustWrite(t, fs, "forge-1.20.1-installer.jar", "old")
	mustWrite(t, fs, TempArchiveName, "crashed download")
	mustWrite(t, fs, StagingDirName+"/left/over.txt", "crashed staging")

	if err := Clean(fs, KindVersion); err != nil {
		t.Fatalf("clean: %v", err)
	}

	assertExists(t, fs, "config/server-settings.toml")
	assertExists(t, fs, "server.properties")
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

	if err := Clean(fs, KindVersion); err != nil {
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

	if err := Clean(fs, KindModpack); err != nil {
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
