package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	iofs "io/fs"
	"sort"
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
// Entries are collected during the walk and sorted before hashing, so the
// digest is a function of the file set and its metadata alone, never of the
// order the kernel happened to enumerate directory entries in.
func (fs *Filesystem) Fingerprint(ctx context.Context, ignoreLines string) (*FingerprintResult, error) {
	start := time.Now()
	matcher := ignore.CompileIgnoreLines(strings.Split(ignoreLines, "\n")...)
	var lines []string
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

		// Ignored directories are pruned; ignored files are simply passed over. A
		// plain file must return nil rather than SkipDir, because WalkDirat treats
		// SkipDir from a file as "stop reading this directory"...
		if matcher.MatchesPath(relative) {
			if d.IsDir() {
				return ufs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			lines = append(lines, fingerprintEntryLine(relative, "dir"))
			return nil
		}

		if d.Type()&iofs.ModeSymlink != 0 {
			lines = append(lines, fingerprintEntryLine(relative, "symlink"))
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		lines = append(lines, fmt.Sprintf("%s\x00%d\x00%d\n", relative, info.Size(), info.ModTime().UnixNano()))
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
	// never changed. Sorting the collected lines before hashing removes that
	// variable, so the digest only ever changes when the file set does...
	sort.Strings(lines)

	digest := sha256.New()
	for _, line := range lines {
		digest.Write([]byte(line))
	}

	return &FingerprintResult{
		Fingerprint: hex.EncodeToString(digest.Sum(nil)),
		Files:       files,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// fingerprintEntryLine formats a non-file entry so that additions and removals
// of directories and links are detected even though they carry no size or mtime.
func fingerprintEntryLine(relative, kind string) string {
	return fmt.Sprintf("%s\x00%s\n", relative, kind)
}
