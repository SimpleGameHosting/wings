package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/franela/goblin"
)

// writeTestFile writes a small file through the filesystem under test.
func writeTestFile(g *G, fs *Filesystem, name, contents string) {
	r := strings.NewReader(contents)
	g.Assert(fs.Write(name, r, r.Size(), 0o644)).IsNil()
}

// fingerprintOf computes a fingerprint with the given ignore lines and fails
// the test on error.
func fingerprintOf(g *G, fs *Filesystem, ignore string) *FingerprintResult {
	result, err := fs.Fingerprint(context.Background(), ignore)
	g.Assert(err).IsNil()
	return result
}

func TestFilesystem_Fingerprint(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Fingerprint", func() {
		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})

		g.It("is deterministic across runs", func() {
			writeTestFile(g, fs, "server.properties", "motd=hello\n")
			writeTestFile(g, fs, "world/level.dat", "level\n")

			first := fingerprintOf(g, fs, "")
			second := fingerprintOf(g, fs, "")

			g.Assert(len(first.Fingerprint)).Equal(64)
			g.Assert(first.Fingerprint).Equal(second.Fingerprint)
			g.Assert(first.Files).Equal(2)
		})

		g.It("changes when a file's size changes", func() {
			writeTestFile(g, fs, "server.properties", "motd=hello\n")
			before := fingerprintOf(g, fs, "")

			writeTestFile(g, fs, "server.properties", "motd=hello world\n")
			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint != after.Fingerprint).IsTrue()
		})

		g.It("changes when only a file's mtime changes", func() {
			writeTestFile(g, fs, "server.properties", "motd=hello\n")
			before := fingerprintOf(g, fs, "")

			past := time.Now().Add(-time.Hour)
			full := filepath.Join(rfs.root, "server", "server.properties")
			g.Assert(os.Chtimes(full, past, past)).IsNil()
			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint != after.Fingerprint).IsTrue()
		})

		g.It("changes when a file is renamed", func() {
			writeTestFile(g, fs, "a.txt", "same\n")
			before := fingerprintOf(g, fs, "")

			g.Assert(fs.Rename("a.txt", "b.txt")).IsNil()
			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint != after.Fingerprint).IsTrue()
		})

		g.It("is independent of directory enumeration order", func() {
			writeTestFile(g, fs, "a.txt", "same\n")
			writeTestFile(g, fs, "b.txt", "same\n")
			writeTestFile(g, fs, "c.txt", "same\n")
			before := fingerprintOf(g, fs, "")

			// Capture each file's mtime, then delete and recreate all three in
			// the opposite order and restore the original mtimes. The file set
			// and its metadata end up identical, so only the kernel's directory
			// enumeration order could still differ between the two runs...
			full := func(name string) string {
				return filepath.Join(rfs.root, "server", name)
			}
			mtimeOf := func(name string) time.Time {
				info, err := os.Stat(full(name))
				g.Assert(err).IsNil()
				return info.ModTime()
			}
			aTime, bTime, cTime := mtimeOf("a.txt"), mtimeOf("b.txt"), mtimeOf("c.txt")

			g.Assert(fs.Delete("a.txt")).IsNil()
			g.Assert(fs.Delete("b.txt")).IsNil()
			g.Assert(fs.Delete("c.txt")).IsNil()

			writeTestFile(g, fs, "c.txt", "same\n")
			writeTestFile(g, fs, "b.txt", "same\n")
			writeTestFile(g, fs, "a.txt", "same\n")

			g.Assert(os.Chtimes(full("a.txt"), aTime, aTime)).IsNil()
			g.Assert(os.Chtimes(full("b.txt"), bTime, bTime)).IsNil()
			g.Assert(os.Chtimes(full("c.txt"), cTime, cTime)).IsNil()

			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint).Equal(after.Fingerprint)
		})

		g.It("ignores changes to files matched by the ignore lines", func() {
			writeTestFile(g, fs, "server.properties", "motd=hello\n")
			writeTestFile(g, fs, "logs/latest.log", "line one\n")
			before := fingerprintOf(g, fs, "logs/\n*.log")

			writeTestFile(g, fs, "logs/latest.log", "line one\nline two\n")
			writeTestFile(g, fs, "debug.log", "noise\n")
			after := fingerprintOf(g, fs, "logs/\n*.log")

			g.Assert(before.Fingerprint).Equal(after.Fingerprint)
			g.Assert(after.Files).Equal(1)
		})

		g.It("does not descend into ignored directories", func() {
			writeTestFile(g, fs, "backups/huge.tar.gz", "x\n")
			writeTestFile(g, fs, "world/level.dat", "level\n")

			result := fingerprintOf(g, fs, "backups/")

			g.Assert(result.Files).Equal(1)
		})

		g.It("changes when an empty directory is added", func() {
			writeTestFile(g, fs, "world/level.dat", "level\n")
			before := fingerprintOf(g, fs, "")

			g.Assert(fs.CreateDirectory("plugins", "/")).IsNil()
			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint != after.Fingerprint).IsTrue()
			g.Assert(after.Files).Equal(before.Files)
		})

		g.It("reports the elapsed time", func() {
			writeTestFile(g, fs, "a.txt", "a\n")

			result := fingerprintOf(g, fs, "")

			g.Assert(result.DurationMs >= 0).IsTrue()
		})

		g.It("stops when the context is cancelled", func() {
			writeTestFile(g, fs, "a.txt", "a\n")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := fs.Fingerprint(ctx, "")

			g.Assert(err).Equal(context.Canceled)
		})
	})
}
