package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/franela/goblin"
	"golang.org/x/sys/unix"
)

// backdateSymlink moves a symlink's own modification time an hour into the
// past without following it, which lets a test tell two versions of the same
// link apart even when both were created inside a single kernel clock tick.
func backdateSymlink(g *G, path string) {
	past := unix.NsecToTimespec(time.Now().Add(-time.Hour).UnixNano())
	g.Assert(unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{past, past}, unix.AT_SYMLINK_NOFOLLOW)).IsNil()
}

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
			writeTestFile(g, fs, "backups/keep.txt", "keep\n")
			writeTestFile(g, fs, "world/level.dat", "level\n")

			// Pruning wins: an ignored directory is never descended into, so the
			// negation naming a file inside it is never reached. That mirrors
			// git's own semantics, and the archiver prunes the same way...
			result := fingerprintOf(g, fs, "backups\n!backups/keep.txt")

			g.Assert(result.Files).Equal(1)
		})

		g.It("matches a trailing-slash pattern per file, exactly as the archiver does", func() {
			writeTestFile(g, fs, "backups/keep.txt", "keep\n")
			writeTestFile(g, fs, "world/level.dat", "level\n")

			// The matcher only recognises "backups/" as a directory pattern when
			// the path it is given also ends in a slash, and the walk offers it
			// the bare relative path. The directory is therefore descended into
			// and every entry is matched individually, so a negation inside it
			// does take effect. The archiver matches the very same way, and the
			// fingerprint must agree with the archiver about what a backup would
			// contain, so this parity is the behaviour worth pinning down...
			result := fingerprintOf(g, fs, "backups/\n!backups/keep.txt")

			g.Assert(result.Files).Equal(2)
		})

		g.It("changes when a symlink is retargeted", func() {
			writeTestFile(g, fs, "target-a", "a\n")
			writeTestFile(g, fs, "target-b", "b\n")

			// Inode timestamps come from a coarse clock that only advances once
			// per kernel tick, so the two links would otherwise be created in the
			// same instant. Backdating the first stands in for the hours that
			// pass between a real backup and a later retarget...
			link := filepath.Join(rfs.root, "server", "link")
			g.Assert(os.Symlink(filepath.Join(rfs.root, "server", "target-a"), link)).IsNil()
			backdateSymlink(g, link)
			before := fingerprintOf(g, fs, "")

			g.Assert(os.Remove(link)).IsNil()
			g.Assert(os.Symlink(filepath.Join(rfs.root, "server", "target-b"), link)).IsNil()
			after := fingerprintOf(g, fs, "")

			g.Assert(before.Fingerprint != after.Fingerprint).IsTrue()
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
