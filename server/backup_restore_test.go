package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/server/backup"
	"github.com/pterodactyl/wings/server/filesystem"
)

// TestBackupRestoreRoundTrip verifies that Wings can restore its generated directory entries and nested files.
func TestBackupRestoreRoundTrip(t *testing.T) {
	server := newResourceTestServer(t)
	sourceFilesystem, err := filesystem.New(filepath.Join(t.TempDir(), "source"), 0, nil)
	require.NoError(t, err)

	// First, create the mix of root files, nested files, and empty directories
	// that exposed the destructive restore regression...
	require.NoError(t, sourceFilesystem.CreateDirectory("region", "/world"))
	require.NoError(t, sourceFilesystem.CreateDirectory("empty", "/plugins"))
	serverPropertiesContents := "motd=Restore Test\n"
	require.NoError(t, sourceFilesystem.Write(
		"server.properties",
		bytes.NewBufferString(serverPropertiesContents),
		int64(len(serverPropertiesContents)),
		0o644,
	))
	regionContents := "region data"
	require.NoError(t, sourceFilesystem.Write(
		"world/region/r.0.0.mca",
		bytes.NewBufferString(regionContents),
		int64(len(regionContents)),
		0o600,
	))
	require.NoError(t, os.Symlink("world/region/r.0.0.mca", filepath.Join(sourceFilesystem.Path(), "region_link")))

	// Next, generate the archive with the same writer used for backups and
	// restore it through the production backup reader and entry handler...
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	require.NoError(t, (&filesystem.Archive{Filesystem: sourceFilesystem}).Create(context.Background(), archivePath))
	archiveFile, err := os.Open(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	require.NoError(t, server.Filesystem().TruncateRootDirectory())
	require.NoError(t, backup.NewS3(nil, testServerUUID, "").Restore(
		context.Background(),
		archiveFile,
		server.restoreBackupEntry,
	))

	// Finally, verify restoration recreated both file contents and the empty
	// directory rather than turning the first directory entry into a file...
	restoredServerProperties, err := os.ReadFile(filepath.Join(server.Filesystem().Path(), "server.properties"))
	require.NoError(t, err)
	require.Equal(t, serverPropertiesContents, string(restoredServerProperties))

	restoredRegion, err := os.ReadFile(filepath.Join(server.Filesystem().Path(), "world/region/r.0.0.mca"))
	require.NoError(t, err)
	require.Equal(t, regionContents, string(restoredRegion))

	emptyDirectory, err := os.Stat(filepath.Join(server.Filesystem().Path(), "plugins/empty"))
	require.NoError(t, err)
	require.True(t, emptyDirectory.IsDir())

	// Symlinks travel through backups as link entries and must come back as
	// links; materializing them as empty files breaks servers such as Forge...
	requireRegionSymlink := func() {
		t.Helper()
		linkInfo, err := os.Lstat(filepath.Join(server.Filesystem().Path(), "region_link"))
		require.NoError(t, err)
		require.NotZero(t, linkInfo.Mode()&os.ModeSymlink, "expected region_link to be restored as a symlink")
		linkTarget, err := os.Readlink(filepath.Join(server.Filesystem().Path(), "region_link"))
		require.NoError(t, err)
		require.Equal(t, "world/region/r.0.0.mca", linkTarget)
	}
	requireRegionSymlink()

	// Restoring the same backup again without truncating first is a supported
	// panel flow and must overwrite the existing link instead of failing...
	archiveFileAgain, err := os.Open(archivePath)
	require.NoError(t, err)
	defer archiveFileAgain.Close()
	require.NoError(t, backup.NewS3(nil, testServerUUID, "").Restore(
		context.Background(),
		archiveFileAgain,
		server.restoreBackupEntry,
	))
	requireRegionSymlink()
}
