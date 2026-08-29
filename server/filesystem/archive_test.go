package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	. "github.com/franela/goblin"
	"github.com/mholt/archives"
)

func TestArchive_Stream(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Archive", func() {
		g.AfterEach(func() {
			// Reset the filesystem after each run.
			_ = fs.TruncateRootDirectory()
		})

		g.It("creates archive with intended files", func() {
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()
			g.Assert(fs.CreateDirectory("test2", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			err := fs.Write("test/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test2/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt.old", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			a := &Archive{
				Filesystem: fs,
				Files: []string{
					"test",
					"test_file.txt",
				},
			}

			// Create the archive.
			archivePath := filepath.Join(rfs.root, "archive.tar.gz")
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			// Ensure the archive exists.
			_, err = os.Stat(archivePath)
			g.Assert(err).IsNil()

			// Open the archive.
			genericFs, err := archives.FileSystem(context.Background(), archivePath, nil)
			g.Assert(err).IsNil()

			// Assert that we are opening an archive.
			afs, ok := genericFs.(iofs.ReadDirFS)
			g.Assert(ok).IsTrue()

			// Get the names of the files recursively from the archive.
			files, err := getFiles(afs, ".")
			g.Assert(err).IsNil()

			// Ensure the files in the archive match what we are expecting.
			expected := []string{
				"test_file.txt",
				"test/file.txt",
			}

			// Sort the slices to ensure the comparison never fails if the
			// contents are sorted differently.
			sort.Strings(expected)
			sort.Strings(files)

			g.Assert(files).Equal(expected)
		})

		g.It("archives symlinks with their target", func() {
			writeTestFile(g, fs, "target.txt", "target\n")
			g.Assert(fs.CreateDirectory("links", "/")).IsNil()
			linkPath := filepath.Join(rfs.root, "server", "links", "link.txt")
			g.Assert(os.Symlink("target.txt", linkPath)).IsNil()

			archivePath := filepath.Join(rfs.root, "archive.tar.gz")
			a := &Archive{Filesystem: fs}
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			file, err := os.Open(archivePath)
			g.Assert(err).IsNil()
			defer file.Close()
			reader, err := gzip.NewReader(file)
			g.Assert(err).IsNil()
			defer reader.Close()

			found := false
			tarReader := tar.NewReader(reader)
			for {
				header, err := tarReader.Next()
				if err == io.EOF {
					break
				}
				g.Assert(err).IsNil()
				if header.Name == "links/link.txt" {
					found = true
					g.Assert(header.Typeflag).Equal(byte(tar.TypeSymlink))
					g.Assert(header.Linkname).Equal("target.txt")
				}
			}

			g.Assert(found).IsTrue()
		})

		g.It("archives empty directories represented by the fingerprint", func() {
			g.Assert(fs.CreateDirectory("empty", "/")).IsNil()

			archivePath := filepath.Join(rfs.root, "archive.tar.gz")
			a := &Archive{Filesystem: fs}
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			file, err := os.Open(archivePath)
			g.Assert(err).IsNil()
			defer file.Close()
			reader, err := gzip.NewReader(file)
			g.Assert(err).IsNil()
			defer reader.Close()

			found := false
			tarReader := tar.NewReader(reader)
			for {
				header, err := tarReader.Next()
				if err == io.EOF {
					break
				}
				g.Assert(err).IsNil()
				if strings.TrimSuffix(header.Name, "/") == "empty" {
					found = true
					g.Assert(header.Typeflag).Equal(byte(tar.TypeDir))
				}
			}

			g.Assert(found).IsTrue()
		})
	})
}

func getFiles(f iofs.ReadDirFS, name string) ([]string, error) {
	var v []string

	entries, err := f.ReadDir(name)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		entryName := e.Name()
		if name != "." {
			entryName = filepath.Join(name, entryName)
		}

		if e.IsDir() {
			files, err := getFiles(f, entryName)
			if err != nil {
				return nil, err
			}

			if files == nil {
				return nil, nil
			}

			v = append(v, files...)
			continue
		}

		v = append(v, entryName)
	}

	return v, nil
}
