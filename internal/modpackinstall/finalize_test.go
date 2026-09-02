package modpackinstall

import "testing"

func TestFinalizeVersionForgeRewritesShimPath(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/minecraftforge/forge/1.20.1-47.3.0"
	mustWrite(t, fs, base+"/unix_args.txt", "-DlegacyClassPath=forge-1.20.1-47.3.0-shim.jar -jar x")
	mustWrite(t, fs, base+"/forge-1.20.1-47.3.0-shim.jar", "shim")

	if err := Finalize(fs, KindVersion, VersionForge); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertContent(t, fs, "unix_args.txt",
		"-DlegacyClassPath=libraries/net/minecraftforge/forge/1.20.1-47.3.0/forge-1.20.1-47.3.0-shim.jar -jar x")
	assertContent(t, fs, "eula.txt", "eula=true\n")
}

// TestFinalizeVersionForgePicksLatestOfTwoDirs proves latestSubdir feeds
// the version-forge rule a real natural-order comparison rather than
// whatever order ReadDir happens to return: 47.10.0 must be picked over
// 47.2.0, which a plain lexical compare would get backwards.
func TestFinalizeVersionForgePicksLatestOfTwoDirs(t *testing.T) {
	fs := newTestFs(t)
	oldBase := "libraries/net/minecraftforge/forge/1.20.1-47.2.0"
	newBase := "libraries/net/minecraftforge/forge/1.20.1-47.10.0"
	mustWrite(t, fs, oldBase+"/unix_args.txt", "-DlegacyClassPath=forge-1.20.1-47.2.0-shim.jar -jar x")
	mustWrite(t, fs, newBase+"/unix_args.txt", "-DlegacyClassPath=forge-1.20.1-47.10.0-shim.jar -jar x")

	if err := Finalize(fs, KindVersion, VersionForge); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertContent(t, fs, "unix_args.txt",
		"-DlegacyClassPath="+newBase+"/forge-1.20.1-47.10.0-shim.jar -jar x")
}

func TestFinalizeModpackForgeSymlinksAndCopiesShim(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/minecraftforge/forge/1.20.1-47.3.0"
	mustWrite(t, fs, base+"/unix_args.txt", "args")
	mustWrite(t, fs, base+"/forge-1.20.1-47.3.0-shim.jar", "shim")

	if err := Finalize(fs, KindModpack, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, "unix_args.txt", base+"/unix_args.txt")
	assertContent(t, fs, "forge-1.20.1-47.3.0-shim.jar", "shim")
}

// TestFinalizeModpackNeoforgeSymlinks covers finalizeModpack's NeoForge
// branch, which unlike Forge's needs only the unix_args.txt symlink and no
// shim copy.
func TestFinalizeModpackNeoforgeSymlinks(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/neoforged/neoforge/21.1.0"
	mustWrite(t, fs, base+"/unix_args.txt", "args")

	if err := Finalize(fs, KindModpack, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, "unix_args.txt", base+"/unix_args.txt")
}

// TestFinalizeVersionNeoforgeSymlinks covers the KindVersion dispatch to
// finalizeVersionNeoforge, a separate call path from the modpack case
// above even though both end up in the shared symlinkUnixArgsTo helper.
func TestFinalizeVersionNeoforgeSymlinks(t *testing.T) {
	fs := newTestFs(t)
	base := "libraries/net/neoforged/neoforge/21.1.0"
	mustWrite(t, fs, base+"/unix_args.txt", "args")

	if err := Finalize(fs, KindVersion, VersionNeoForge); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	assertSymlink(t, fs, "unix_args.txt", base+"/unix_args.txt")
}

func TestFinalizeRefusesInstallerJar(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "installer.jar", "evil")

	if err := Finalize(fs, KindVersion, VersionForge); err == nil {
		t.Fatal("expected installer.jar to fail the install")
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
	assertContent(t, fs, "server.jar", "a")

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
	mustWrite(t, fs, "server.jar", "already-placed")

	if err := Finalize(fs, KindVersion, VersionSponge); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	assertContent(t, fs, "server.jar", "already-placed")
}

func TestFinalizeFabricRename(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "fabric-server-launch.jar", "fab")

	if err := Finalize(fs, KindVersion, VersionFabric); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	assertContent(t, fs, "server.jar", "fab")
}

func TestCompareVersionish(t *testing.T) {
	if compareVersionish("1.20.1-47.3.0", "1.20.1-47.10.0") >= 0 {
		t.Error("47.10 must sort above 47.3")
	}
	if compareVersionish("9.0", "10.0") >= 0 {
		t.Error("10 must sort above 9")
	}
}
