package filesystem

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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
			g.Assert(targetFs.ExtractStreamUnsafe(context.Background(), "/", &archiveBuffer)).IsNil()

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
