package modpackinstall

import (
	"path"
	"strings"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/filesystem"
)

// versionCleanDirs are the root-level directories the version profile
// removes, mirrored from the SGH Version Installer egg script: loader state
// goes, configs and world data stay.
var versionCleanDirs = []string{"libraries", "mods", "coremods", ".fabric", ".neoforge"}

// versionCleanFiles are the root-level loader files the version profile
// removes alongside versionCleanDirs.
var versionCleanFiles = []string{"user_jvm_args.txt", "server.jar", "unix_args.txt", "run.sh", "run.bat"}

// versionCleanJarPrefixes are root-level jar name prefixes left behind by
// loader installers of any era. The list is kept exactly as the legacy egg
// script defines it, so a stray jar from an install that predates the
// current version_type enum is still swept.
var versionCleanJarPrefixes = []string{"forge-", "fabric-", "paper-", "purpur-", "spigot-", "velocity-", "bungeecord-", "waterfall-"}

// Clean prepares a server's root directory for an install. A modpack install
// gets a full wipe since the archive owns the entire tree; a version install
// only loses the previous loader's files so configs and world data survive.
// Both profiles always sweep the fixed install artifacts so a crashed
// earlier attempt cannot strand disk usage or stale bytes.
func Clean(fs *filesystem.Filesystem, kind Kind) error {
	switch kind {
	case KindModpack:
		return cleanEverything(fs)
	case KindVersion:
		return cleanVersionProfile(fs)
	default:
		return errors.Errorf("modpackinstall: unknown kind %q", kind)
	}
}

// cleanEverything deletes every entry at the server root, matching today's
// panel-side clearServerFiles combined with the egg script's own rm list.
func cleanEverything(fs *filesystem.Filesystem) error {
	entries, err := fs.ReadDir("/")
	if err != nil {
		return err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return deleteAll(fs, names)
}

// cleanVersionProfile deletes only the previous loader's files, leaving
// configs, worlds, and server.properties untouched.
func cleanVersionProfile(fs *filesystem.Filesystem) error {
	targets := append([]string{}, versionCleanDirs...)
	targets = append(targets, versionCleanFiles...)
	targets = append(targets, TempArchiveName, StagingDirName)

	// The jar prefixes are root-level globs in the egg script rather than
	// fixed names, so walk the root once and collect whatever currently
	// matches before deleting anything...
	entries, err := fs.ReadDir("/")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		for _, prefix := range versionCleanJarPrefixes {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".jar") {
				targets = append(targets, name)
				break
			}
		}
	}

	return deleteAll(fs, targets)
}

// deleteAll removes each named root entry, tolerating entries that are
// already absent since the goal is the same clean end state either way. It
// goes through the filesystem's own Delete so quota accounting and path
// sandboxing stay correct instead of touching disk directly.
func deleteAll(fs *filesystem.Filesystem, names []string) error {
	for _, name := range names {
		if err := fs.Delete(path.Join("/", name)); err != nil && !isNotExist(err) {
			return err
		}
	}

	return nil
}

// isNotExist reports whether err represents a path that was already absent.
func isNotExist(err error) bool {
	return errors.Is(err, ufs.ErrNotExist)
}
