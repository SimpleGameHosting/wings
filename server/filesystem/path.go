package filesystem

import (
	"path/filepath"
	"strings"

	"emperror.dev/errors"
)

// Checks if the given file or path is in the server's file denylist. If so, an Error
// is returned, otherwise nil is returned.
//
// TODO: update logic to use unixFS
func (fs *Filesystem) IsIgnored(paths ...string) error {
	for _, p := range paths {
		// Match the path the filesystem will actually touch, not the string the
		// client sent. Rooting the path before cleaning mirrors how the unixFS
		// clamps every path under the server root, so traversal (foo/../denied),
		// a leading ".." that the clamp swallows, doubled slashes, and dot
		// segments all collapse to the same denied location...
		cleaned := filepath.Clean("/" + p)
		if fs.denylist.MatchesPath(cleaned) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: cleaned})
		}
	}
	return nil
}

// Generate a path to the file by cleaning it up and appending the root server path to it. This
// DOES NOT guarantee that the file resolves within the server data directory. You'll want to use
// the fs.unsafeIsInDataDirectory(p) function to confirm.
func (fs *Filesystem) unsafeFilePath(p string) string {
	// Calling filepath.Clean on the joined directory will resolve it to the absolute path,
	// removing any ../ type of resolution arguments, and leaving us with a direct path link.
	//
	// This will also trim the existing root path off the beginning of the path passed to
	// the function since that can get a bit messy.
	return filepath.Clean(filepath.Join(fs.Path(), strings.TrimPrefix(p, fs.Path())))
}

// Check that that path string starts with the server data directory path. This function DOES NOT
// validate that the rest of the path does not end up resolving out of this directory, or that the
// targeted file or folder is not a symlink doing the same thing.
func (fs *Filesystem) unsafeIsInDataDirectory(p string) bool {
	return strings.HasPrefix(strings.TrimSuffix(p, "/")+"/", strings.TrimSuffix(fs.Path(), "/")+"/")
}
