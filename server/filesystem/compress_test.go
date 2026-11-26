package filesystem

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/franela/goblin"
)

// Given an archive named test.{ext}, with the following file structure:
//
//	test/
//	|──inside/
//	|────finside.txt
//	|──outside.txt
//
// this test will ensure that it's being decompressed as expected
func TestFilesystem_DecompressFile(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Decompress", func() {
		for _, ext := range []string{"zip", "rar", "tar", "tar.gz"} {
			g.It("can decompress a "+ext, func() {
				// copy the file to the new FS
				c, err := os.ReadFile("./testdata/test." + ext)
				g.Assert(err).IsNil()
				err = rfs.CreateServerFile("./test."+ext, c)
				g.Assert(err).IsNil()

				// decompress
				err = fs.DecompressFile(context.Background(), "/", "test."+ext)
				g.Assert(err).IsNil()

				// make sure everything is where it is supposed to be
				_, err = rfs.StatServerFile("test/outside.txt")
				g.Assert(err).IsNil()

				st, err := rfs.StatServerFile("test/inside")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = rfs.StatServerFile("test/inside/finside.txt")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()
			})
		}

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

// Empty directories have no file to create them implicitly, so extraction must
// create them explicitly or they are dropped.
func TestFilesystem_DecompressFileEmptyDirectory(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Decompress", func() {
		archives := []struct {
			name  string
			build func() ([]byte, error)
		}{
			{"empty.zip", zipWithEmptyDir},
			{"empty.tar.gz", tarGzWithEmptyDir},
		}

		for _, a := range archives {
			g.It("preserves an empty directory in a "+a.name, func() {
				content, err := a.build()
				g.Assert(err).IsNil()
				err = rfs.CreateServerFile("./"+a.name, content)
				g.Assert(err).IsNil()

				err = fs.DecompressFile(context.Background(), "/", a.name)
				g.Assert(err).IsNil()

				// The empty directory must exist, and the sibling file must still extract.
				st, err := rfs.StatServerFile("empty")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = rfs.StatServerFile("outside.txt")
				g.Assert(err).IsNil()
			})
		}

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

// zipWithEmptyDir builds a zip holding one file and an empty directory ("empty/").
func zipWithEmptyDir() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	dh := &zip.FileHeader{Name: "empty/"}
	dh.SetMode(os.ModeDir | 0o755)
	if _, err := zw.CreateHeader(dh); err != nil {
		return nil, err
	}

	w, err := zw.Create("outside.txt")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Symlinked files are a normal part of game servers (Forge boots through a
// symlinked unix_args.txt), so extraction must recreate them as links rather
// than materializing empty regular files at the link path.
func TestFilesystem_ExtractStreamSymlinks(t *testing.T) {
	g := Goblin(t)
	sourceFs, _ := NewFs()
	targetFs, _ := NewFs()

	g.Describe("ExtractStream", func() {
		g.AfterEach(func() {
			_ = sourceFs.TruncateRootDirectory()
			_ = targetFs.TruncateRootDirectory()
		})

		g.It("preserves symlinks through an archive round trip", func() {
			contents := strings.NewReader("-jar forge.jar")
			g.Assert(sourceFs.Write("libraries/forge/unix_args.txt", contents, contents.Size(), 0o644)).IsNil()
			g.Assert(os.Symlink("libraries/forge/unix_args.txt", filepath.Join(sourceFs.Path(), "unix_args.txt"))).IsNil()
			g.Assert(os.Symlink("/etc/hostname", filepath.Join(sourceFs.Path(), "absolute_link"))).IsNil()
			g.Assert(os.Symlink("missing_target.txt", filepath.Join(sourceFs.Path(), "dangling_link"))).IsNil()

			// Stream the archive with the production archiver into memory and
			// extract it into a second filesystem exactly like a transfer...
			var archiveBuffer bytes.Buffer
			g.Assert((&Archive{Filesystem: sourceFs}).Stream(context.Background(), &archiveBuffer)).IsNil()
			archiveBytes := archiveBuffer.Bytes()
			g.Assert(targetFs.ExtractStreamUnsafe(context.Background(), "/", bytes.NewReader(archiveBytes))).IsNil()

			// Extracting the same archive again must overwrite the links it
			// created the first time instead of failing with "file exists"...
			g.Assert(targetFs.ExtractStreamUnsafe(context.Background(), "/", bytes.NewReader(archiveBytes))).IsNil()

			expectSymlink := func(name, expectedTarget string) {
				info, err := os.Lstat(filepath.Join(targetFs.Path(), name))
				g.Assert(err).IsNil()
				g.Assert(info.Mode()&os.ModeSymlink != 0).IsTrue("expected " + name + " to be a symlink")
				target, err := os.Readlink(filepath.Join(targetFs.Path(), name))
				g.Assert(err).IsNil()
				g.Assert(target).Equal(expectedTarget)
			}
			expectSymlink("unix_args.txt", "libraries/forge/unix_args.txt")
			expectSymlink("absolute_link", "/etc/hostname")
			expectSymlink("dangling_link", "missing_target.txt")

			restored, err := os.ReadFile(filepath.Join(targetFs.Path(), "libraries/forge/unix_args.txt"))
			g.Assert(err).IsNil()
			g.Assert(string(restored)).Equal("-jar forge.jar")
		})

		g.It("preserves symlinks stored in a zip archive", func() {
			content, err := zipWithSymlink()
			g.Assert(err).IsNil()
			g.Assert(targetFs.Write("linked.zip", bytes.NewReader(content), int64(len(content)), 0o644)).IsNil()

			// A stale regular file at the link path must be replaced by the
			// archived symlink, the same way file entries overwrite files...
			stale := strings.NewReader("stale regular file")
			g.Assert(targetFs.Write("config_link", stale, stale.Size(), 0o644)).IsNil()

			g.Assert(targetFs.DecompressFile(context.Background(), "/", "linked.zip")).IsNil()

			info, err := os.Lstat(filepath.Join(targetFs.Path(), "config_link"))
			g.Assert(err).IsNil()
			g.Assert(info.Mode()&os.ModeSymlink != 0).IsTrue("expected config_link to be a symlink")
			target, err := os.Readlink(filepath.Join(targetFs.Path(), "config_link"))
			g.Assert(err).IsNil()
			g.Assert(target).Equal("data/config.txt")
		})
	})
}

// TestFilesystem_TryStartDecompression covers the single-flight reservation
// used to stop concurrent background decompressions of the same archive.
func TestFilesystem_TryStartDecompression(t *testing.T) {
	fs, _ := NewFs()

	release, ok := fs.TryStartDecompression("/", "bundle.tar.gz")
	if !ok {
		t.Fatal("expected the first reservation to succeed")
	}
	if _, ok := fs.TryStartDecompression("/", "bundle.tar.gz"); ok {
		t.Fatal("expected a duplicate reservation to be refused")
	}

	otherRelease, ok := fs.TryStartDecompression("/other", "bundle.tar.gz")
	if !ok {
		t.Fatal("expected a different archive path to reserve independently")
	}
	otherRelease()

	release()
	releaseAgain, ok := fs.TryStartDecompression("/", "bundle.tar.gz")
	if !ok {
		t.Fatal("expected the archive to be reservable again after release")
	}
	releaseAgain()
}

// zipWithSymlink builds a zip holding a real file and a symlink pointing at it.
func zipWithSymlink() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	w, err := zw.Create("data/config.txt")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte("port=25565")); err != nil {
		return nil, err
	}

	lh := &zip.FileHeader{Name: "config_link"}
	lh.SetMode(os.ModeSymlink | 0o777)
	lw, err := zw.CreateHeader(lh)
	if err != nil {
		return nil, err
	}
	if _, err := lw.Write([]byte("data/config.txt")); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tarGzWithEmptyDir builds a tar.gz holding one file and an empty directory ("empty/").
func tarGzWithEmptyDir() ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{Name: "empty/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, err
	}

	content := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{Name: "outside.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestFilesystem_ReserveArchive(t *testing.T) {
	fs, rfs := NewFs()

	first, releaseFirst, ok := fs.ReserveArchive("/", []string{"a", "b"})
	if !ok {
		t.Fatal("expected the first reservation to succeed")
	}
	if _, _, ok := fs.ReserveArchive("/", []string{"b", "a"}); ok {
		t.Fatal("expected the same paths in a different order to be refused")
	}

	second, releaseSecond, ok := fs.ReserveArchive("/", []string{"a"})
	if !ok {
		t.Fatal("expected a different set to reserve independently")
	}
	third, releaseThird, ok := fs.ReserveArchive("/", []string{"b"})
	if !ok {
		t.Fatal("expected another different set to reserve independently")
	}
	if first == second || second == third || first == third {
		t.Fatalf("expected unique names, got %q %q %q", first, second, third)
	}
	releaseSecond()
	releaseThird()

	releaseFirst()
	if err := rfs.CreateServerFileFromString(first, "taken"); err != nil {
		t.Fatal(err)
	}
	again, releaseAgain, ok := fs.ReserveArchive("/", []string{"a", "b"})
	if !ok {
		t.Fatal("expected the set to be reservable again after release")
	}
	defer releaseAgain()
	if again == first || !strings.HasSuffix(again, ".tar.gz") {
		t.Fatalf("expected a fresh name different from %q, got %q", first, again)
	}
	if !strings.HasSuffix(again, "-2.tar.gz") {
		t.Fatalf("expected the existing name to get a -2 suffix, got %q", again)
	}
}

func TestFilesystem_WriteArchive(t *testing.T) {
	fs, rfs := NewFs()
	if err := rfs.CreateServerFileFromString("hello.txt", "hello world"); err != nil {
		t.Fatal(err)
	}

	name, release, ok := fs.ReserveArchive("/", []string{"hello.txt"})
	if !ok {
		t.Fatal("expected the reservation to succeed")
	}
	defer release()

	info, err := fs.WriteArchive(context.Background(), "/", []string{"hello.txt"}, name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != name {
		t.Fatalf("expected info for %q, got %q", name, info.Name())
	}
	if _, err := rfs.StatServerFile(name + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("expected the partial file to be gone, got %v", err)
	}

	file, err := os.Open(filepath.Join(rfs.root, "server", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "hello.txt" {
		t.Fatalf("expected hello.txt in the archive, got %q", header.Name)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello world" {
		t.Fatalf("unexpected archived content %q", body)
	}
}

func TestFilesystem_WriteArchiveRemovesPartialOnCancel(t *testing.T) {
	fs, rfs := NewFs()
	if err := rfs.CreateServerFileFromString("hello.txt", "hello world"); err != nil {
		t.Fatal(err)
	}

	name, release, _ := fs.ReserveArchive("/", []string{"hello.txt"})
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fs.WriteArchive(ctx, "/", []string{"hello.txt"}, name); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	assertArchiveFilesAbsent(t, rfs, name)
}

func TestFilesystem_WriteArchiveRemovesPartialWhenQuotaExceeded(t *testing.T) {
	fs, rfs := NewFs()
	if err := rfs.CreateServerFileFromString("hello.txt", strings.Repeat("hello world ", 100)); err != nil {
		t.Fatal(err)
	}
	fs.SetDiskLimit(1)

	name, release, _ := fs.ReserveArchive("/", []string{"hello.txt"})
	defer release()

	_, err := fs.WriteArchive(context.Background(), "/", []string{"hello.txt"}, name)
	if !IsErrorCode(err, ErrCodeDiskSpace) {
		t.Fatalf("expected a disk space error, got %v", err)
	}
	assertArchiveFilesAbsent(t, rfs, name)
}

func assertArchiveFilesAbsent(t *testing.T, rfs *rootFs, name string) {
	t.Helper()
	for _, candidate := range []string{name, name + ".partial"} {
		if _, err := rfs.StatServerFile(candidate); !os.IsNotExist(err) {
			t.Fatalf("expected %q to not exist, got %v", candidate, err)
		}
	}
}
