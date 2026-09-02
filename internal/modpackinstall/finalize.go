package modpackinstall

import (
	"io"
	"path"
	"strconv"
	"strings"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/filesystem"
)

const (
	eulaFileName          = "eula.txt"
	eulaContent           = "eula=true\n"
	installerJarName      = "installer.jar"
	unixArgsFileName      = "unix_args.txt"
	serverJarName         = "server.jar"
	fabricServerLaunchJar = "fabric-server-launch.jar"
	forgeLibraryDir       = "libraries/net/minecraftforge/forge"
	neoforgeLibraryDir    = "libraries/net/neoforged/neoforge"
)

// Finalize applies the per-kind, per-loader fixups that the tails of both
// SGH egg install scripts performed after extraction, translated into pure
// Go so Wings never has to shell out to Java to prepare a server for its
// first boot. Both kinds refuse an install that still carries a
// version-archive installer.jar and then always publish eula.txt, before
// dispatching to the modpack- or version-specific rules.
func Finalize(fs *filesystem.Filesystem, kind Kind, vt VersionType) error {
	if err := refuseInstallerJar(fs); err != nil {
		return err
	}
	if err := writeEula(fs); err != nil {
		return err
	}

	switch kind {
	case KindModpack:
		return finalizeModpack(fs)
	case KindVersion:
		return finalizeVersion(fs, vt)
	default:
		return errors.Errorf("modpackinstall: unknown kind %q", kind)
	}
}

// finalizeModpack applies the SGH Modpack Installer egg script's tail. A
// modern Forge layout gets its unix_args.txt symlinked into place plus its
// launch shim copied to the root; a modern NeoForge layout only needs the
// symlink. The two checks run independently rather than as mutually
// exclusive branches, mirroring the original script, but the legacy
// fallback below is itself guarded so it never fires once a loader has
// already resolved unix_args.txt.
func finalizeModpack(fs *filesystem.Filesystem) error {
	// First, look for a modern Forge install and link its launch files in
	// if one is present...
	forgeVersion, err := latestSubdir(fs, forgeLibraryDir)
	switch {
	case err == nil:
		if err := linkModpackForge(fs, forgeVersion); err != nil {
			return err
		}
	case !errors.Is(err, ufs.ErrNotExist):
		return err
	}

	// Next, do the same for a modern NeoForge install; finding one ends the
	// function here since NeoForge needs nothing further...
	neoforgeVersion, err := latestSubdir(fs, neoforgeLibraryDir)
	switch {
	case err == nil:
		return symlinkUnixArgsTo(fs, path.Join(neoforgeLibraryDir, neoforgeVersion))
	case !errors.Is(err, ufs.ErrNotExist):
		return err
	}

	// Neither modern layout was found, so fall back to the legacy
	// standalone forge-*.jar rename; it no-ops on its own if Forge already
	// resolved unix_args.txt above...
	return legacyForgeJarFallback(fs)
}

// linkModpackForge symlinks root unix_args.txt to the installed Forge
// version's own copy and copies its launch shim jar to the root when
// present, matching the modpack egg script's Forge branch.
func linkModpackForge(fs *filesystem.Filesystem, version string) error {
	base := path.Join(forgeLibraryDir, version)

	if err := symlinkUnixArgsTo(fs, base); err != nil {
		return err
	}

	return copyModpackForgeShim(fs, base, version)
}

// copyModpackForgeShim copies the Forge version's launch shim jar to the
// server root when the installer produced one, using cp semantics: the
// source stays under libraries/, since the version-install path also needs
// it there.
func copyModpackForgeShim(fs *filesystem.Filesystem, base, version string) error {
	shimName := "forge-" + version + "-shim.jar"
	shimPath := path.Join(base, shimName)

	exists, err := pathExists(fs, shimPath)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	if err := copyFile(fs, shimPath, shimName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to copy forge shim jar to root")
	}
	return nil
}

// legacyForgeJarFallback handles an old-style modpack archive that dropped
// a bare universal Forge jar at the root instead of the modern
// libraries/net/minecraftforge/forge/<version> layout. It only fires when
// neither a loader symlink nor an existing server.jar already resolved the
// launch target. Finding no candidate simply means this was not a legacy
// Forge modpack, so it is left untouched; finding more than one is
// ambiguous and fails loudly rather than guessing the way the reference
// shell script's unconditional first-match rename did.
func legacyForgeJarFallback(fs *filesystem.Filesystem) error {
	unixArgsPresent, err := pathExists(fs, unixArgsFileName)
	if err != nil {
		return err
	}
	serverJarPresent, err := pathExists(fs, serverJarName)
	if err != nil {
		return err
	}
	if unixArgsPresent || serverJarPresent {
		return nil
	}

	jars, err := rootJars(fs)
	if err != nil {
		return err
	}

	var candidates []string
	for _, name := range jars {
		if strings.HasPrefix(name, "forge-") && !strings.HasSuffix(name, "-installer.jar") {
			candidates = append(candidates, name)
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) > 1 {
		return errors.Errorf("modpackinstall: multiple candidate forge jars at root, refusing to guess: %v", candidates)
	}

	if err := fs.Replace(candidates[0], serverJarName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to rename forge jar to server.jar")
	}
	return nil
}

// finalizeVersion applies the SGH Version Installer egg script's tail,
// which differs per loader: Forge needs its launch arguments rewritten
// with an absolute shim path, NeoForge only needs a symlink, Fabric
// renames its launcher jar, and every other known type just needs
// server.jar in place. An unrecognized VersionType is refused outright
// rather than falling through to generic handling, even though Request.
// Validate should already have ruled that out before Finalize ever runs.
func finalizeVersion(fs *filesystem.Filesystem, vt VersionType) error {
	switch vt {
	case VersionForge:
		return finalizeVersionForge(fs)
	case VersionNeoForge:
		return finalizeVersionNeoforge(fs)
	case VersionFabric:
		return finalizeVersionFabric(fs)
	case VersionVanilla, VersionSnapshot, VersionPaper, VersionPurpur, VersionSponge, VersionVelocity:
		return finalizeVersionGeneric(fs)
	default:
		return errors.Errorf("modpackinstall: unknown version_type %q", vt)
	}
}

// finalizeVersionForge rewrites the installed Forge loader's unix_args.txt
// into a real root-level file with its shim jar path fully qualified,
// since a KindVersion install runs from the server root rather than from
// inside libraries/net/minecraftforge/forge/<version>, where Forge's own
// installer wrote a path relative to itself.
func finalizeVersionForge(fs *filesystem.Filesystem) error {
	version, err := latestSubdir(fs, forgeLibraryDir)
	if err != nil {
		return errors.Wrap(err, "modpackinstall: failed to locate installed forge version")
	}
	base := path.Join(forgeLibraryDir, version)

	content, err := readFileString(fs, path.Join(base, unixArgsFileName))
	if err != nil {
		return errors.Wrap(err, "modpackinstall: failed to read forge unix_args.txt")
	}
	rewritten := rewriteForgeShimPath(content, base, version)

	if err := fs.Write(unixArgsFileName, strings.NewReader(rewritten), int64(len(rewritten)), 0o644); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to write unix_args.txt")
	}
	return nil
}

// rewriteForgeShimPath substitutes the bare forge-<version>-shim.jar token
// Forge's installer writes into unix_args.txt with its real path under
// libraries/, so the JVM can still find the shim jar when launched from
// the server root. The already-qualified check guards against
// double-prefixing content that (for whatever reason) already carries the
// base directory.
func rewriteForgeShimPath(content, base, version string) string {
	shimName := "forge-" + version + "-shim.jar"
	qualified := path.Join(base, shimName)

	if strings.Contains(content, base+"/forge-") {
		return content
	}
	return strings.ReplaceAll(content, shimName, qualified)
}

// finalizeVersionNeoforge publishes root unix_args.txt as a symlink into
// the installed NeoForge version's own copy. Unlike Forge, NeoForge's
// unix_args.txt needs no path rewriting, so a symlink is enough.
func finalizeVersionNeoforge(fs *filesystem.Filesystem) error {
	version, err := latestSubdir(fs, neoforgeLibraryDir)
	if err != nil {
		return errors.Wrap(err, "modpackinstall: failed to locate installed neoforge version")
	}
	return symlinkUnixArgsTo(fs, path.Join(neoforgeLibraryDir, version))
}

// finalizeVersionFabric renames Fabric's launcher jar to server.jar when
// the installer produced one under that name; newer Fabric installers may
// already name their output server.jar, so absence here is not an error.
func finalizeVersionFabric(fs *filesystem.Filesystem) error {
	exists, err := pathExists(fs, fabricServerLaunchJar)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	if err := fs.Replace(fabricServerLaunchJar, serverJarName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to rename fabric-server-launch.jar")
	}
	return nil
}

// finalizeVersionGeneric handles every KindVersion type that is not
// Forge, NeoForge, or Fabric. Jar-direct types (vanilla, snapshot, paper,
// purpur, velocity) already have server.jar in place from PlaceJar, so
// this is a pass-through for them; a generic archive type such as Sponge
// only gets server.jar once its single extracted root jar is renamed
// here. The legacy script's nondeterministic pick among several jars is
// deliberately not ported: ambiguity fails loudly instead of guessing.
func finalizeVersionGeneric(fs *filesystem.Filesystem) error {
	exists, err := pathExists(fs, serverJarName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	jars, err := rootJars(fs)
	if err != nil {
		return err
	}
	if len(jars) != 1 {
		return errors.Errorf("modpackinstall: expected exactly one root jar to become server.jar, found %d", len(jars))
	}

	if err := fs.Replace(jars[0], serverJarName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to rename jar to server.jar")
	}
	return nil
}

// writeEula publishes the fixed eula.txt content every successful install
// needs, mirroring both egg scripts' unconditional `echo eula=true`.
func writeEula(fs *filesystem.Filesystem) error {
	if err := fs.Write(eulaFileName, strings.NewReader(eulaContent), int64(len(eulaContent)), 0o644); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to write eula.txt")
	}
	return nil
}

// refuseInstallerJar fails the install loudly when a Forge/NeoForge
// installer.jar made it to the root. Version archives are pre-built by the
// panel and Wings must never run Java to execute one.
func refuseInstallerJar(fs *filesystem.Filesystem) error {
	exists, err := pathExists(fs, installerJarName)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("modpackinstall: refusing to finalize: installer.jar present, Wings never runs Java")
	}
	return nil
}

// symlinkUnixArgsTo publishes root unix_args.txt as a symlink into dir,
// the shape every loader whose own launch arguments need no rewriting
// (both NeoForge branches, and Forge's modpack branch) shares.
func symlinkUnixArgsTo(fs *filesystem.Filesystem, dir string) error {
	target := path.Join(dir, unixArgsFileName)
	if err := fs.OverwriteSymlink(target, unixArgsFileName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to symlink unix_args.txt")
	}
	return nil
}

// copyFile duplicates src's bytes to dst inside fs without removing src,
// matching the reference scripts' `cp` semantics. Replace, by contrast,
// moves the entry and would leave nothing behind under libraries/.
func copyFile(fs *filesystem.Filesystem, src, dst string) error {
	f, st, err := fs.File(src)
	if err != nil {
		return err
	}
	defer f.Close()

	return fs.Write(dst, f, st.Size(), 0o644)
}

// readFileString reads p's entire content into a string and closes the
// handle before returning, for the small text files finalize rewrites.
func readFileString(fs *filesystem.Filesystem, p string) (string, error) {
	f, _, err := fs.File(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// pathExists reports whether p is present on fs, treating a missing entry
// as a plain false rather than an error so callers can write simple
// presence checks.
func pathExists(fs *filesystem.Filesystem, p string) (bool, error) {
	if _, err := fs.UnixFS().Lstat(p); err != nil {
		if errors.Is(err, ufs.ErrNotExist) {
			return false, nil
		}
		return false, errors.Wrap(err, "modpackinstall: failed to check for "+p)
	}
	return true, nil
}

// rootJars lists the *.jar file names sitting directly at the server root,
// the candidate pool the jar-guessing finalize rules narrow down from.
func rootJars(fs *filesystem.Filesystem) ([]string, error) {
	entries, err := fs.ReadDir("/")
	if err != nil {
		return nil, errors.Wrap(err, "modpackinstall: failed to read server root")
	}

	var jars []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jar") {
			jars = append(jars, entry.Name())
		}
	}
	return jars, nil
}

// latestSubdir returns the name of dir's immediate subdirectory that sorts
// last under compareVersionish, the same "pick the newest installed
// version" intent as the shell scripts' `sort -V | tail -n1`. It reports
// ufs.ErrNotExist (checkable with errors.Is) when dir itself is absent, so
// callers can tell "this loader was never installed" apart from a genuine
// read failure.
func latestSubdir(fs *filesystem.Filesystem, dir string) (string, error) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var latest string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if latest == "" || compareVersionish(entry.Name(), latest) > 0 {
			latest = entry.Name()
		}
	}
	if latest == "" {
		return "", errors.Errorf("modpackinstall: %q has no version subdirectories", dir)
	}

	return latest, nil
}

// compareVersionish performs a three-way natural-order comparison of two
// version-like strings, splitting on '.' and '-' and comparing purely
// numeric segments as numbers rather than text. This mirrors the intent of
// the shell scripts' `sort -V | tail -n1` idiom (Forge and NeoForge both
// publish subdirectories such as "1.20.1-47.10.0", where a plain string
// compare would incorrectly rank ".2." above ".10.") without depending on
// an external sort binary. It returns a negative number when a sorts
// before b, zero when they are equivalent, and a positive number when a
// sorts after b.
func compareVersionish(a, b string) int {
	segmentsA := splitVersionSegments(a)
	segmentsB := splitVersionSegments(b)

	for i := 0; i < len(segmentsA) && i < len(segmentsB); i++ {
		if cmp := compareVersionSegment(segmentsA[i], segmentsB[i]); cmp != 0 {
			return cmp
		}
	}

	return len(segmentsA) - len(segmentsB)
}

// splitVersionSegments breaks a version-like string into its dot- and
// dash-separated parts, e.g. "1.20.1-47.10.0" becomes
// ["1", "20", "1", "47", "10", "0"].
func splitVersionSegments(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-'
	})
}

// compareVersionSegment compares a single pair of segments numerically
// when both parse as plain non-negative integers, falling back to a plain
// string compare for anything else (pre-release-style suffixes, empty
// segments, and the like) so no input can make compareVersionish panic.
func compareVersionSegment(a, b string) int {
	numA, errA := strconv.Atoi(a)
	numB, errB := strconv.Atoi(b)
	if errA == nil && errB == nil {
		return numA - numB
	}
	return strings.Compare(a, b)
}
