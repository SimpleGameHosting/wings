package modpackinstall

import (
	"testing"

	"github.com/pterodactyl/wings/server/filesystem"
)

// forgeEraCase describes one of the three Forge layout eras the shared
// finalize tail has to recognize: what the extracted server root looks
// like going in, and what it must look like coming out.
type forgeEraCase struct {
	name   string
	setup  func(t *testing.T, fs *filesystem.Filesystem)
	assert func(t *testing.T, fs *filesystem.Filesystem)
}

// forge121Dir is the library path of a Forge 1.21+ install, whose root
// server.jar is the bootstrap shim the launch command uses directly.
const forge121Dir = "libraries/net/minecraftforge/forge/1.21.1-52.0.0"

// forge120Dir is the library path of a Forge 1.17-1.20 install, whose own
// unix_args.txt is what the root symlink has to point at.
const forge120Dir = "libraries/net/minecraftforge/forge/1.20.1-47.3.0"

// forgeEraCases are the three eras of the live egg scripts' shared Forge
// tail, as pinned by panel migration 2026_03_03_000001_fix_forge_21_unix_args:
// 1.21+ is left alone because its root server.jar already boots, 1.17-1.20
// gets root unix_args.txt symlinked into the installed version, and
// pre-1.17 has its single root forge jar renamed to server.jar.
var forgeEraCases = []forgeEraCase{
	{
		name: "forge 1.21+ keeps its server.jar bootstrap and publishes no unix_args",
		setup: func(t *testing.T, fs *filesystem.Filesystem) {
			mustWrite(t, fs, forge121Dir+"/unix_args.txt", "args")
			mustWrite(t, fs, serverJarName, "forge121shim")
		},
		assert: func(t *testing.T, fs *filesystem.Filesystem) {
			// Publishing unix_args.txt here is exactly the bug the March
			// 2026 migration fixed: the shim's Class-Path resolves
			// relative to the jar, so the module path breaks...
			assertMissing(t, fs, unixArgsFileName)
			assertContent(t, fs, serverJarName, "forge121shim")
		},
	},
	{
		name: "forge 1.17-1.20 symlinks root unix_args into the installed version",
		setup: func(t *testing.T, fs *filesystem.Filesystem) {
			mustWrite(t, fs, forge120Dir+"/unix_args.txt", "args")
		},
		assert: func(t *testing.T, fs *filesystem.Filesystem) {
			assertSymlink(t, fs, unixArgsFileName, forge120Dir+"/unix_args.txt")
			assertMissing(t, fs, serverJarName)
		},
	},
	{
		name: "legacy pre-1.17 renames the single root forge jar to server.jar",
		setup: func(t *testing.T, fs *filesystem.Filesystem) {
			mustWrite(t, fs, "forge-1.12.2-14.23.5.2860-universal.jar", "legacyforge")
		},
		assert: func(t *testing.T, fs *filesystem.Filesystem) {
			assertContent(t, fs, serverJarName, "legacyforge")
			assertMissing(t, fs, "forge-1.12.2-14.23.5.2860-universal.jar")
			assertMissing(t, fs, unixArgsFileName)
		},
	},
}

// TestFinalizeForgeErasModpack proves a modpack install applies all three
// eras of the live script's Forge tail.
func TestFinalizeForgeErasModpack(t *testing.T) {
	for _, tc := range forgeEraCases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newTestFs(t)
			tc.setup(t, fs)

			if err := Finalize(fs, KindModpack, ""); err != nil {
				t.Fatalf("finalize: %v", err)
			}

			tc.assert(t, fs)
			assertContent(t, fs, "eula.txt", "eula=true\n")
		})
	}
}

// TestFinalizeForgeErasVersion proves a version install applies the same
// three eras, since both egg scripts share one Forge tail.
func TestFinalizeForgeErasVersion(t *testing.T) {
	for _, tc := range forgeEraCases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newTestFs(t)
			tc.setup(t, fs)

			if err := Finalize(fs, KindVersion, VersionForge); err != nil {
				t.Fatalf("finalize: %v", err)
			}

			tc.assert(t, fs)
			assertContent(t, fs, "eula.txt", "eula=true\n")
		})
	}
}

// TestFinalizeForgePicksLatestOfTwoDirs proves latestSubdir feeds the
// shared Forge rule a real natural-order comparison rather than whatever
// order ReadDir happens to return: 47.10.0 must be picked over 47.2.0,
// which a plain lexical compare would get backwards.
func TestFinalizeForgePicksLatestOfTwoDirs(t *testing.T) {
	fs := newTestFs(t)
	oldBase := "libraries/net/minecraftforge/forge/1.20.1-47.2.0"
	newBase := "libraries/net/minecraftforge/forge/1.20.1-47.10.0"
	mustWrite(t, fs, oldBase+"/unix_args.txt", "old")
	mustWrite(t, fs, newBase+"/unix_args.txt", "new")

	if err := Finalize(fs, KindVersion, VersionForge); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, unixArgsFileName, newBase+"/unix_args.txt")
}

// TestFinalizeForgeToleratesEmptyForgeDirectory covers the `-n` guard both
// scripts carry: a forge library directory with no version subdirectory,
// which an unpacked-but-empty archive can leave behind, must not fail the
// install, it must simply fall through to the legacy rule.
func TestFinalizeForgeToleratesEmptyForgeDirectory(t *testing.T) {
	fs := newTestFs(t)
	if err := fs.CreateDirectory("forge", "libraries/net/minecraftforge"); err != nil {
		t.Fatalf("create empty forge dir: %v", err)
	}
	mustWrite(t, fs, "forge-1.12.2-14.23.5.2860-universal.jar", "legacyforge")

	if err := Finalize(fs, KindModpack, ""); err != nil {
		t.Fatalf("an empty forge directory must not fail the install: %v", err)
	}

	assertContent(t, fs, serverJarName, "legacyforge")
}

// TestFinalizeModpackNeoforgeSymlinks covers the shared tail's NeoForge
// branch, which unlike Forge's needs only the unix_args.txt symlink.
func TestFinalizeModpackNeoforgeSymlinks(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/neoforged/neoforge/21.1.0"
	mustWrite(t, fs, base+"/unix_args.txt", "args")

	if err := Finalize(fs, KindModpack, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, unixArgsFileName, base+"/unix_args.txt")
}

// TestFinalizeVersionNeoforgeSymlinks covers the KindVersion dispatch into
// that same tail, a separate call path from the modpack case above.
func TestFinalizeVersionNeoforgeSymlinks(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/neoforged/neoforge/21.1.0"
	mustWrite(t, fs, base+"/unix_args.txt", "args")

	if err := Finalize(fs, KindVersion, VersionNeoForge); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, unixArgsFileName, base+"/unix_args.txt")
}

// TestFinalizeRefusesInstallerJar pins Ruling 19: the never-run-Java
// posture applies to both kinds, not just version archives.
func TestFinalizeRefusesInstallerJar(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind Kind
		vt   VersionType
	}{
		{"version", KindVersion, VersionForge},
		{"modpack", KindModpack, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newTestFs(t)
			mustWrite(t, fs, "installer.jar", "evil")

			if err := Finalize(fs, tc.kind, tc.vt); err == nil {
				t.Fatal("expected installer.jar to fail the install")
			}
		})
	}
}

// TestFinalizeGenericRequiresExactlyOneJar exercises the generic
// finalize rule (Controller Ruling 2 for Task 13: VersionSponge stands in
// for the brief's VersionSpigot, since sponge is the real generic-archive
// enum value; the fixture filename is left as spigot-1.21.4.jar because
// the one-jar rule ignores names entirely).
func TestFinalizeGenericRequiresExactlyOneJar(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "spigot-1.21.4.jar", "a")

	if err := Finalize(fs, KindVersion, VersionSponge); err != nil {
		t.Fatalf("one jar should pass: %v", err)
	}
	assertContent(t, fs, serverJarName, "a")

	fs2 := newTestFs(t)
	mustWrite(t, fs2, "a.jar", "a")
	mustWrite(t, fs2, "b.jar", "b")
	if err := Finalize(fs2, KindVersion, VersionSponge); err == nil {
		t.Fatal("two jars must fail loudly")
	}
}

// TestFinalizeGenericPassesThroughWhenServerJarPresent covers the
// jar-direct types (vanilla, snapshot, paper, purpur, velocity): PlaceJar
// already wrote server.jar before Finalize runs, so the generic rule must
// leave it alone rather than looking for a root jar to rename.
func TestFinalizeGenericPassesThroughWhenServerJarPresent(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, serverJarName, "already-placed")

	if err := Finalize(fs, KindVersion, VersionSponge); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	assertContent(t, fs, serverJarName, "already-placed")
}

func TestFinalizeFabricRename(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "fabric-server-launch.jar", "fab")

	if err := Finalize(fs, KindVersion, VersionFabric); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	assertContent(t, fs, serverJarName, "fab")
}

func TestCompareVersionish(t *testing.T) {
	if compareVersionish("1.20.1-47.3.0", "1.20.1-47.10.0") >= 0 {
		t.Error("47.10 must sort above 47.3")
	}
	if compareVersionish("9.0", "10.0") >= 0 {
		t.Error("10 must sort above 9")
	}
}
