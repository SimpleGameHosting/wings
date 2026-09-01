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
