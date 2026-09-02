package modpackinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	"github.com/pterodactyl/wings/server/filesystem"
)

// writeTarGz builds a small tar.gz at TempArchiveName inside the test fs, so
// extraction tests have a real archive to decompress rather than a hand-built
// staging tree.
func writeTarGz(t *testing.T, fs *filesystem.Filesystem, entries map[string]string, symlinks map[string]string) {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	for name, target := range symlinks {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: target})
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := fs.Write(TempArchiveName, &buf, int64(buf.Len()), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
}

func TestExtractAndSettleMergesIntoExisting(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "config/existing.toml", "keep-me")
	mustWrite(t, fs, "config/overwritten.toml", "old")
	writeTarGz(t, fs, map[string]string{
		"mods/alpha.jar":          "jarbytes",
		"config/overwritten.toml": "new",
		"._junk":                  "macos",
	}, map[string]string{"unix_args.txt": "libraries/args.txt"})

	if err := ExtractToStaging(context.Background(), fs); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := Settle(fs); err != nil {
		t.Fatalf("settle: %v", err)
	}

	assertContent(t, fs, "config/existing.toml", "keep-me")
	assertContent(t, fs, "config/overwritten.toml", "new")
	assertContent(t, fs, "mods/alpha.jar", "jarbytes")
	assertMissing(t, fs, "._junk")
	assertMissing(t, fs, StagingDirName)
	assertMissing(t, fs, TempArchiveName)
	assertSymlink(t, fs, "unix_args.txt", "libraries/args.txt")
}

func TestPlaceJar(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, TempArchiveName, "paperbytes")
	mustWrite(t, fs, "server.jar", "oldjar")

	if err := PlaceJar(fs, "server.jar"); err != nil {
		t.Fatalf("place: %v", err)
	}

	assertContent(t, fs, "server.jar", "paperbytes")
	assertMissing(t, fs, TempArchiveName)
}

// TestSettleOverwritesFileWithDirectory ports mmi-install-binary's
// moveDirectory case where the destination already holds a plain file: the
// file has to be deleted before the staged directory can take its place,
// exactly as moveDirectory's os.RemoveAll did ahead of the merge.
func TestSettleOverwritesFileWithDirectory(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "shared", "old-file-content")
	mustWrite(t, fs, StagingDirName+"/shared/inner.txt", "new-inner")

	if err := Settle(fs); err != nil {
		t.Fatalf("settle: %v", err)
	}

	assertContent(t, fs, "shared/inner.txt", "new-inner")

	info, err := fs.UnixFS().Lstat("shared")
	if err != nil {
		t.Fatalf("lstat shared: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected %q to be a directory after settle, mode was %v", "shared", info.Mode())
	}
}

// TestSettleOverwritesDirectoryWithFile ports mmi-install-binary's moveFile
// case where the destination already holds a directory: the whole directory
// has to be removed before the staged file can take its place, exactly as
// moveFile's unconditional os.RemoveAll did ahead of the rename.
func TestSettleOverwritesDirectoryWithFile(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "config.txt/nested.txt", "old-nested")
	mustWrite(t, fs, StagingDirName+"/config.txt", "new-file-content")

	if err := Settle(fs); err != nil {
		t.Fatalf("settle: %v", err)
	}

	assertContent(t, fs, "config.txt", "new-file-content")
}

// TestSettleSymlinkOverwritesNonEmptyDirectory ports the same overwrite
// contract to a symlink entry: OverwriteSymlink alone refuses a non-empty
// directory, so a staged symlink has to settle through the same
// clear-then-place path a regular file uses.
func TestSettleSymlinkOverwritesNonEmptyDirectory(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, "link.txt/nested.txt", "old-nested")
	if err := fs.Symlink("target.txt", StagingDirName+"/link.txt"); err != nil {
		t.Fatalf("stage symlink: %v", err)
	}

	if err := Settle(fs); err != nil {
		t.Fatalf("settle: %v", err)
	}

	assertSymlink(t, fs, "link.txt", "target.txt")
}
