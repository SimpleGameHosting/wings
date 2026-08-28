package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	iofs "io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/internal/ufs"
)

// FingerprintResult describes the state of the files a backup would include,
// reduced to a single digest the panel can compare between runs.
type FingerprintResult struct {
	Fingerprint string `json:"fingerprint"`
	Files       int    `json:"files"`
	DurationMs  int64  `json:"duration_ms"`
}

// Fingerprint walks the server root and digests the path, size and modification
// time of every file that a backup honouring the given ignore lines would include.
// It reads no file contents, so its cost is comparable to the periodic disk usage
// walk. The ignore lines use the same matcher as the archiver, which keeps the
// fingerprint and the archive in agreement about what counts as server content.
// Like the archiver, the walk never prunes a directory it matches against the
// ignore list; it descends into it regardless and matches every entry inside
// individually, so a negated pattern that re-includes a single file under an
// otherwise ignored directory is honoured the same way by both the fingerprint
// and the archive. Each entry is reduced to its own SHA-256 digest as it is
// visited, and those fixed-size digests are sorted before being folded into the
// final hash. The fingerprint is therefore a function of the file set and its
// metadata alone, never of the order the kernel happened to enumerate directory
// entries in, and the walk holds a flat 32 bytes per entry however long the
// paths are.
func (fs *Filesystem) Fingerprint(ctx context.Context, ignoreLines string) (*FingerprintResult, error) {
	start := time.Now()
	matcher := ignore.CompileIgnoreLines(strings.Split(ignoreLines, "\n")...)
	var digests [][sha256.Size]byte
	files := 0

	// Open the root the same way the archiver does so both walks see the same tree...
	dirfd, name, closeFd, err := fs.unixFS.SafePath("")
	defer closeFd()
	if err != nil {
		return nil, err
	}

	err = fs.unixFS.WalkDirat(dirfd, name, func(_ int, _ string, relative string, d ufs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// The root entry itself carries no information about server content...
		if relative == "." {
			return nil
		}

		// The walk deliberately never prunes: the archiver itself descends into
		// every directory regardless of the ignore matcher, applying the matcher
		// only to individual entry paths as it goes. A directory pattern such as
		// "backups" together with a negation like "!backups/keep.txt" therefore
		// still lets the archiver include keep.txt. If this walk pruned the
		// backups directory outright, keep.txt's content could change without
		// moving the fingerprint, which is the unsafe direction. So an ignored
		// directory is simply left out of the digest and the walk continues into
		// it, matching every file inside individually exactly as the archiver
		// does...
		if matcher.MatchesPath(relative) {
			return nil
		}

		if d.IsDir() {
			digests = append(digests, hashEntry(relative, "dir"))
			return nil
		}

		// A symlink is described by its own lstat modification time, so pointing
		// an existing link at a new target changes the fingerprint even though
		// the link's path stays the same...
		if d.Type()&iofs.ModeSymlink != 0 {
			info, err := d.Info()
			if err != nil {
				return err
			}

			digests = append(digests, hashEntry(relative, "symlink", info.ModTime().UnixNano()))

			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		// A file carries no kind token: its size takes that slot, so the line reads
		// "path\x00size\x00mtime\n"...
		digests = append(digests, hashEntry(relative, "", info.Size(), info.ModTime().UnixNano()))
		files++

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Hashed-directory filesystems such as ext4 (htree) and btrfs do not
	// guarantee stable enumeration order: an unrelated insert or delete
	// elsewhere in a directory can reshuffle the order later entries are
	// returned in, hours apart, even though the directory's own contents
	// never changed. Sorting the entry digests before folding them together
	// removes that variable, so the fingerprint only ever changes when the
	// file set does...
	slices.SortFunc(digests, func(a, b [sha256.Size]byte) int {
		return bytes.Compare(a[:], b[:])
	})

	digest := sha256.New()
	for _, entry := range digests {
		digest.Write(entry[:])
	}

	return &FingerprintResult{
		Fingerprint: hex.EncodeToString(digest.Sum(nil)),
		Files:       files,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// hashEntry reduces one walked entry to its fixed-size digest. Building the
// line with append instead of fmt keeps the per-entry cost to one small
// allocation even on trees with hundreds of thousands of entries.
//
// The line is the entry's path, then an optional kind token, then each number,
// all NUL-separated and closed by a newline. A file passes an empty kind so its
// size occupies that slot, which is what reproduces the three layouts the
// fingerprint has always used: "path\x00size\x00mtime\n" for a file,
// "path\x00dir\n" for a directory and "path\x00symlink\x00mtime\n" for a
// symlink. These bytes are the fingerprint's on-the-wire format across panel
// comparisons, so they must never change without invalidating every stored
// fingerprint.
func hashEntry(relative, kind string, numbers ...int64) [sha256.Size]byte {
	line := make([]byte, 0, len(relative)+len(kind)+2+len(numbers)*21)
	line = append(line, relative...)

	if kind != "" {
		line = append(line, 0)
		line = append(line, kind...)
	}

	for _, n := range numbers {
		line = append(line, 0)
		line = strconv.AppendInt(line, n, 10)
	}

	line = append(line, '\n')

	return sha256.Sum256(line)
}
