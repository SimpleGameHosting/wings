package modpackinstall

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/filesystem"
)

// ExtractToStaging extracts the downloaded temp archive into the staging
// directory ahead of settling it into the server root. DecompressFile only
// ever extracts an archive in place alongside itself, so the archive is
// relocated into staging first; its format is then sniffed from content by
// the filesystem layer, which also preserves symlinks and enforces the disk
// quota while extracting.
func ExtractToStaging(ctx context.Context, fs *filesystem.Filesystem) error {
	// The staging directory has to exist before anything can be extracted
	// into it...
	if err := fs.CreateDirectory(StagingDirName, "/"); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to create staging directory")
	}

	stagedArchive := path.Join(StagingDirName, TempArchiveName)
	if err := fs.Replace(TempArchiveName, stagedArchive); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to move the artifact into staging")
	}

	if err := fs.SpaceAvailableForDecompression(ctx, StagingDirName, TempArchiveName); err != nil {
		return errors.Wrap(err, "modpackinstall: not enough space to extract the artifact")
	}

	if err := fs.DecompressFile(ctx, StagingDirName, TempArchiveName); err != nil {
		return errors.Wrap(err, "modpackinstall: extraction failed")
	}

	// The archive itself is not part of the install; drop it now so settle
	// only ever finds genuine extracted content inside staging...
	if err := fs.Delete(stagedArchive); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to remove the staged artifact")
	}

	return nil
}

// Settle merge-moves everything staged into the server root and then removes
// the install artifacts, leaving the server directory as though the archive
// had always lived there. The merge semantics are ported from
// mmi-install-binary's mover.go: files publish over whatever they replace,
// directories merge recursively into an existing directory, an existing
// entry of the wrong type is deleted before the staged one takes its place,
// symlinks are recreated at the destination rather than followed, and macOS
// "._" resource-fork junk is dropped instead of copied.
func Settle(fs *filesystem.Filesystem) error {
	if err := settleDir(fs, StagingDirName, "/"); err != nil {
		return err
	}

	// Both install artifacts are always swept once settling has finished, so
	// a later install attempt never finds stale bytes left behind...
	if err := fs.Delete(StagingDirName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to remove staging directory")
	}
	if err := fs.Delete(TempArchiveName); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to remove temp archive")
	}

	return nil
}

// PlaceJar publishes the downloaded temp archive as a raw jar artifact under
// its runtime name (always "server.jar"), replacing whatever jar sat there
// before.
func PlaceJar(fs *filesystem.Filesystem, target string) error {
	if err := fs.Replace(TempArchiveName, target); err != nil {
		return errors.Wrap(err, "modpackinstall: failed to place jar")
	}

	return nil
}

// settleDir merge-moves every entry of src into dst, recursing into staged
// directories so an existing destination directory is merged into rather
// than replaced outright.
func settleDir(fs *filesystem.Filesystem, src, dst string) error {
	entries, err := fs.ReadDir(src)
	if err != nil {
		return errors.Wrap(err, "modpackinstall: settle read failed")
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "._") {
			continue // macOS resource-fork junk, dropped like the reference installer did
		}

		srcPath := path.Join(src, name)
		dstPath := path.Join(dst, name)

		switch {
		case entry.Type()&ufs.ModeSymlink != 0:
			if err := settleSymlink(fs, srcPath, dstPath); err != nil {
				return err
			}
		case entry.IsDir():
			if err := settleDirEntry(fs, srcPath, dstPath, name, dst); err != nil {
				return err
			}
		default:
			if err := settleFileEntry(fs, srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// settleFileEntry publishes a single staged file at dstPath. A directory
// currently occupying that path is cleared first, since neither Replace nor
// the rename it relies on can place a file where a directory stands.
func settleFileEntry(fs *filesystem.Filesystem, srcPath, dstPath string) error {
	if err := clearDirectoryConflict(fs, dstPath); err != nil {
		return err
	}

	// Replace renames the staged file into place and keeps quota accounting
	// correct for whatever file it overwrote...
	if err := fs.Replace(srcPath, dstPath); err != nil {
		return errors.Wrap(err, "modpackinstall: settle move failed")
	}

	return nil
}

// settleDirEntry ensures a staged directory exists at dstPath and merges its
// contents into it. A file currently occupying that path is cleared first,
// since a directory can never be created where a file stands.
func settleDirEntry(fs *filesystem.Filesystem, srcPath, dstPath, name, dst string) error {
	if err := clearFileConflict(fs, dstPath); err != nil {
		return err
	}

	if err := fs.CreateDirectory(name, dst); err != nil {
		return errors.Wrap(err, "modpackinstall: settle mkdir failed")
	}

	return settleDir(fs, srcPath, dstPath)
}

// settleSymlink recreates a staged symlink at dstPath by reading its own
// target text rather than following it, since a symlink's target need not
// exist on disk yet, whether because the archive never included it or
// because this same install has not placed it there yet.
func settleSymlink(fs *filesystem.Filesystem, srcPath, dstPath string) error {
	target, err := readSymlinkTarget(fs, srcPath)
	if err != nil {
		return errors.Wrap(err, "modpackinstall: settle readlink failed")
	}

	if err := fs.OverwriteSymlink(target, dstPath); err != nil {
		return errors.Wrap(err, "modpackinstall: settle symlink failed")
	}

	return nil
}

// readSymlinkTarget returns the raw link text of the symlink at
// relativePath, without following it. The sandboxed filesystem type exposes
// no readlink accessor of its own, so this reads the link directly by
// joining the filesystem's own base path; relativePath always comes from a
// directory listing this package already produced, so it can never carry a
// traversal segment.
func readSymlinkTarget(fs *filesystem.Filesystem, relativePath string) (string, error) {
	return os.Readlink(filepath.Join(fs.Path(), relativePath))
}

// clearFileConflict removes a file occupying dstPath so a staged directory
// can be created there, mirroring moveDirectory's os.RemoveAll ahead of the
// merge in the reference installer's mover.go.
func clearFileConflict(fs *filesystem.Filesystem, dstPath string) error {
	return clearIfWrongType(fs, dstPath, true)
}

// clearDirectoryConflict removes a directory occupying dstPath so a staged
// file can be placed there, mirroring moveFile's os.RemoveAll ahead of the
// rename in the reference installer's mover.go.
func clearDirectoryConflict(fs *filesystem.Filesystem, dstPath string) error {
	return clearIfWrongType(fs, dstPath, false)
}

// clearIfWrongType deletes whatever sits at dstPath when it is not the type
// about to be placed there. It does nothing when the path is already absent,
// or is already the right type, since that case is a normal overwrite or
// merge rather than a conflict.
func clearIfWrongType(fs *filesystem.Filesystem, dstPath string, placingDirectory bool) error {
	info, err := fs.UnixFS().Lstat(dstPath)
	if err != nil {
		if errors.Is(err, ufs.ErrNotExist) {
			return nil
		}
		return errors.Wrap(err, "modpackinstall: settle stat failed")
	}

	if info.IsDir() == placingDirectory {
		return nil
	}

	if err := fs.Delete(dstPath); err != nil {
		return errors.Wrap(err, "modpackinstall: settle clear failed")
	}

	return nil
}
