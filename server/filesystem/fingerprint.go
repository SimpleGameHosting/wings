package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	iofs "io/fs"
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
func (fs *Filesystem) Fingerprint(ctx context.Context, ignoreLines string) (*FingerprintResult, error) {
	start := time.Now()
	matcher := ignore.CompileIgnoreLines(strings.Split(ignoreLines, "\n")...)
	digest := sha256.New()
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
			writeFingerprintEntry(digest, relative, "dir")
			return nil
		}

		if d.Type()&iofs.ModeSymlink != 0 {
			writeFingerprintEntry(digest, relative, "symlink")
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		fmt.Fprintf(digest, "%s\x00%d\x00%d\n", relative, info.Size(), info.ModTime().UnixNano())
		files++
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &FingerprintResult{
		Fingerprint: hex.EncodeToString(digest.Sum(nil)),
		Files:       files,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// writeFingerprintEntry records a non-file entry so that additions and removals
// of directories and links are detected even though they carry no size or mtime.
func writeFingerprintEntry(digest hash.Hash, relative, kind string) {
	fmt.Fprintf(digest, "%s\x00%s\n", relative, kind)
}
