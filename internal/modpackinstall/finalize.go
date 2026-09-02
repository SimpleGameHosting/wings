package modpackinstall

import (
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

// errLoaderNotInstalled reports that a mod loader's library directory is
// either absent or carries no version subdirectory, which is how both egg
// scripts read an empty `ls ... | sort -V | tail -1`. It is a "this loader
// was simply not installed" signal rather than a failure, so finalize can
// move on to the next rule instead of aborting the install.
var errLoaderNotInstalled = errors.Sentinel("modpackinstall: loader is not installed")

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
		// A modpack archive carries whatever loader it was built against,
		// so the shared Forge/NeoForge tail is the whole of its finalize...
		return finalizeForge(fs)
	case KindVersion:
		return finalizeVersion(fs, vt)
	default:
		return errors.Errorf("modpackinstall: unknown kind %q", kind)
	}
}

// finalizeForge is the one Forge/NeoForge tail both egg install scripts
// share, as pinned by panel migration 2026_03_03_000001_fix_forge_21_unix_args.
// It recognizes three eras of Forge layout in order. Forge 1.21+ ships a
// root server.jar that is itself the bootstrap shim, so nothing is
// published for it; Forge 1.17-1.20 needs root unix_args.txt symlinked
// into the installed version's own copy; and a pre-1.17 pack that dropped
// a bare universal jar at the root has that jar renamed to server.jar. A
// NeoForge install always just needs the symlink. Every step tolerates its
// loader being absent, since an install only ever carries one of them.
func finalizeForge(fs *filesystem.Filesystem) error {
	// First, resolve a modern Forge layout if the archive brought one...
	forgeVersion, err := latestSubdir(fs, forgeLibraryDir)
	switch {
	case err == nil:
		if err := linkForgeUnixArgs(fs, forgeVersion); err != nil {
			return err
		}
	case !errors.Is(err, errLoaderNotInstalled):
		return err
	}

	// Next, do the same for a modern NeoForge install, whose own launch
	// arguments need no rewriting and so end the tail here...
	neoforgeVersion, err := latestSubdir(fs, neoforgeLibraryDir)
	switch {
	case err == nil:
		return symlinkUnixArgsTo(fs, path.Join(neoforgeLibraryDir, neoforgeVersion))
	case !errors.Is(err, errLoaderNotInstalled):
		return err
	}

	// Neither modern layout resolved a launch target, so fall back to the
	// legacy standalone forge-*.jar rename; it no-ops on its own when
	// something above already put one in place...
	return legacyForgeJarFallback(fs)
}

// linkForgeUnixArgs publishes root unix_args.txt for a modern Forge
// install, unless this is Forge 1.21+. From 1.21 on, Forge's installer
// leaves a root server.jar whose manifest Class-Path resolves relative to
// the jar's own location, so adding @unix_args.txt on top of it hides the
// securemodules jar and the server dies on a missing
// UnionFileSystemProvider; those installs are left exactly as extracted.
func linkForgeUnixArgs(fs *filesystem.Filesystem, version string) error {
	base := path.Join(forgeLibraryDir, version)

	// The rule only applies when the installed version actually published
	// launch arguments of its own to point at...
	argsPresent, err := pathExists(fs, path.Join(base, unixArgsFileName))
	if err != nil {
		return err
	}
	if !argsPresent {
		return nil
	}

	bootstrapPresent, err := pathExists(fs, serverJarName)
	if err != nil {
		return err
	}
	if bootstrapPresent {
		return nil
	}

	return symlinkUnixArgsTo(fs, base)
}

// legacyForgeJarFallback handles an old-style pack or version archive that
// dropped a bare universal Forge jar at the root instead of the modern
// libraries/net/minecraftforge/forge/<version> layout. It only fires when
// neither a loader symlink nor an existing server.jar already resolved the
// launch target. Finding no candidate simply means this was not a legacy
// Forge install, so it is left untouched; finding more than one is
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
// which differs per loader: Forge and NeoForge run the same shared tail as
// a modpack does, Fabric renames its launcher jar, and every other known
// type just needs server.jar in place. An unrecognized VersionType is
// refused outright rather than falling through to generic handling, even
// though Request.Validate should already have ruled that out before
// Finalize ever runs.
func finalizeVersion(fs *filesystem.Filesystem, vt VersionType) error {
	switch vt {
	case VersionForge, VersionNeoForge:
		// The script's `forge|neoforge)` branch runs one tail for both,
		// checking each loader's library directory in turn...
		return finalizeForge(fs)
	case VersionFabric:
		return finalizeVersionFabric(fs)
	case VersionVanilla, VersionSnapshot, VersionPaper, VersionPurpur, VersionSponge, VersionVelocity:
		return finalizeVersionGeneric(fs)
	default:
		return errors.Errorf("modpackinstall: unknown version_type %q", vt)
	}
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
// installer.jar made it to the root. Both modpack and version archives are
// pre-built by the panel and Wings must never run Java to execute one.
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
// (Forge 1.17-1.20 and NeoForge) shares.
func symlinkUnixArgsTo(fs *filesystem.Filesystem, dir string) error {
	target := path.Join(dir, unixArgsFileName)
	if err := fs.OverwriteSymlink(target, unixArgsFileName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to symlink unix_args.txt")
	}
	return nil
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
// version" intent as the shell scripts' `sort -V | tail -n1`. A missing
// directory and a directory holding no version subdirectory both report
// errLoaderNotInstalled (checkable with errors.Is), the Go stand-in for
// the scripts' `-n "$FORGE_VERSION"` guard, so neither shape fails an
// install that simply used a different loader.
func latestSubdir(fs *filesystem.Filesystem, dir string) (string, error) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, ufs.ErrNotExist) {
			return "", errLoaderNotInstalled
		}
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
		return "", errLoaderNotInstalled
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
