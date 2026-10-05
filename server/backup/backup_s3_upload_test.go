package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

// fakeS3 is a stand-in for an S3 endpoint that accepts presigned UploadPart
// requests. Each part URL carries its part number, and the handler hook decides
// how every attempt is answered.
type fakeS3 struct {
	server *httptest.Server

	mu       sync.Mutex
	received map[int][]byte

	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	handle func(w http.ResponseWriter, r *http.Request, part int, body []byte)
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()

	f := &fakeS3{received: map[int][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			highest := f.maxInFlight.Load()
			if current <= highest || f.maxInFlight.CompareAndSwap(highest, current) {
				break
			}
		}

		var part int
		if _, err := fmt.Sscanf(r.URL.Query().Get("partNumber"), "%d", &part); err != nil {
			http.Error(w, "missing partNumber", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}

		if f.handle != nil {
			f.handle(w, r, part, body)
			return
		}
		f.accept(w, part, body)
	}))
	t.Cleanup(f.server.Close)

	return f
}

// accept stores the part and answers like S3 does, with the part's ETag.
func (f *fakeS3) accept(w http.ResponseWriter, part int, body []byte) {
	f.mu.Lock()
	f.received[part] = body
	f.mu.Unlock()

	w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, part))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) uploadURLs(parts int, partSize int64) remote.BackupRemoteUploadResponse {
	urls := make([]string, parts)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/bucket/archive.tar.gz?uploadId=abc&partNumber=%d", f.server.URL, i+1)
	}

	return remote.BackupRemoteUploadResponse{Parts: urls, PartSize: partSize}
}

func (f *fakeS3) part(number int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.received[number]
}

// archiveOf returns a deterministic archive where every byte shows its own position,
// so a part read from the wrong offset can never match.
func archiveOf(size int) []byte {
	archive := make([]byte, size)
	for i := range archive {
		archive[i] = byte(i % 251)
	}

	return archive
}

func uploadArchive(t *testing.T, ctx context.Context, s3 *fakeS3, limits *uploadLimits, archive []byte, partSize int64) ([]remote.BackupPart, error) {
	t.Helper()

	parts := (int64(len(archive)) + partSize - 1) / partSize
	upload := s3.uploadURLs(int(parts), partSize)

	return uploadParts(ctx, http.DefaultClient, limits, bytes.NewReader(archive), upload, int64(len(archive)), log.WithField("test", t.Name()))
}

// Each part must carry exactly its own byte range, including the shorter last part,
// and the panel must get the ETags back in part order to complete the upload.
func TestUploadPartsSendsEachPartsBytesAndReturnsETagsInOrder(t *testing.T) {
	s3 := newFakeS3(t)
	archive := archiveOf(25)

	parts, err := uploadArchive(t, context.Background(), s3, newUploadLimits(4, 0), archive, 10)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	want := []remote.BackupPart{
		{ETag: `"etag-1"`, PartNumber: 1},
		{ETag: `"etag-2"`, PartNumber: 2},
		{ETag: `"etag-3"`, PartNumber: 3},
	}
	if fmt.Sprint(parts) != fmt.Sprint(want) {
		t.Fatalf("expected parts %v, got %v", want, parts)
	}
	for number, wantBytes := range map[int][]byte{1: archive[0:10], 2: archive[10:20], 3: archive[20:25]} {
		if !bytes.Equal(s3.part(number), wantBytes) {
			t.Fatalf("part %d: expected bytes %v, got %v", number, wantBytes, s3.part(number))
		}
	}
}

// Parts of one backup upload at the same time, but every backup on the node shares
// the same slots, so two backups together never exceed the node's limit.
func TestUploadPartsShareTheNodeSlotsAcrossBackups(t *testing.T) {
	s3 := newFakeS3(t)
	// Holding every part briefly lets concurrent requests overlap on the server.
	s3.handle = func(w http.ResponseWriter, r *http.Request, part int, body []byte) {
		time.Sleep(50 * time.Millisecond)
		s3.accept(w, part, body)
	}
	limits := newUploadLimits(3, 0)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := uploadArchive(t, context.Background(), s3, limits, archiveOf(80), 10)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("upload failed: %v", err)
		}
	}

	if got := s3.maxInFlight.Load(); got != 3 {
		t.Fatalf("expected the node to run exactly 3 parts at once, got %d", got)
	}
}

// A zero or negative concurrency from config.yml must still upload, one part at a
// time, instead of waiting forever for a slot that can never exist.
func TestUploadPartsTreatsAMissingConcurrencyAsOne(t *testing.T) {
	s3 := newFakeS3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := uploadArchive(t, ctx, s3, newUploadLimits(0, 0), archiveOf(30), 10); err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if got := s3.maxInFlight.Load(); got != 1 {
		t.Fatalf("expected one part at a time, got %d", got)
	}
}

// A backup cancelled while its parts wait for slots must hand back every slot it
// took, or the node slowly loses upload slots until Wings restarts.
func TestUploadPartsReturnsEverySlotWhenCancelled(t *testing.T) {
	s3 := newFakeS3(t)
	limits := newUploadLimits(1, 0)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if _, err := uploadArchive(t, cancelled, s3, limits, archiveOf(30), 10); err == nil {
			t.Fatal("expected a cancelled upload to fail")
		}
	}

	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if _, err := uploadArchive(t, ctx, s3, limits, archiveOf(30), 10); err != nil {
		t.Fatalf("expected the node's slot to be free again, got %v", err)
	}
}

// A part that S3 rejects with a 5xx is retried, and the retry must send the whole
// part again rather than whatever was left of the first attempt's body.
func TestUploadPartsRetriesAPartWithItsFullBytes(t *testing.T) {
	s3 := newFakeS3(t)
	var attempts atomic.Int32
	s3.handle = func(w http.ResponseWriter, r *http.Request, part int, body []byte) {
		if part == 1 && attempts.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		s3.accept(w, part, body)
	}
	archive := archiveOf(20)

	if _, err := uploadArchive(t, context.Background(), s3, newUploadLimits(1, 0), archive, 10); err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected part 1 to be attempted twice, got %d", got)
	}
	if !bytes.Equal(s3.part(1), archive[0:10]) {
		t.Fatalf("expected the retry to send %v, got %v", archive[0:10], s3.part(1))
	}
}

// When one part is refused for good, the backup has failed, so the parts still
// uploading are cancelled instead of tying up the node's slots and bandwidth.
func TestUploadPartsCancelsTheOtherPartsWhenOneFails(t *testing.T) {
	s3 := newFakeS3(t)
	cancelled := make(chan struct{}, 4)
	s3.handle = func(w http.ResponseWriter, r *http.Request, part int, body []byte) {
		if part == 1 {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-time.After(10 * time.Second):
			s3.accept(w, part, body)
		}
	}

	started := time.Now()
	_, err := uploadArchive(t, context.Background(), s3, newUploadLimits(3, 0), archiveOf(30), 10)

	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("expected the failure to stop the other parts promptly, took %s", elapsed)
	}
	if len(cancelled) != 2 {
		t.Fatalf("expected parts 2 and 3 to be cancelled, got %d cancellations", len(cancelled))
	}
}

// The speed cap is shared by every part on the node, so uploading 3 MiB at 1 MiB/s
// cannot finish in under about two seconds however many parts run at once.
func TestUploadPartsStayUnderTheNodeSpeedLimit(t *testing.T) {
	s3 := newFakeS3(t)
	const mebibyte = 1024 * 1024

	started := time.Now()
	if _, err := uploadArchive(t, context.Background(), s3, newUploadLimits(3, 1), archiveOf(3*mebibyte), mebibyte); err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	// The bucket starts full, so the first second's worth goes out at once and the
	// other two mebibytes take at least two seconds; 1.5 leaves room for timer slack.
	if elapsed := time.Since(started); elapsed < 1500*time.Millisecond {
		t.Fatalf("expected the 1 MiB/s limit to hold the upload to about 2s, finished in %s", elapsed)
	}
}

// uploadURLClient is a Panel client that hands out presigned part URLs from a fake S3,
// with a part size small enough to split a test archive into many parts.
type uploadURLClient struct {
	remote.Client
	s3       *fakeS3
	partSize int64
}

func (c *uploadURLClient) GetBackupRemoteUploadURLs(_ context.Context, _ string, size int64) (remote.BackupRemoteUploadResponse, error) {
	parts := (size + c.partSize - 1) / c.partSize

	return c.s3.uploadURLs(int(parts), c.partSize), nil
}

// A real backup, archived from a server directory and uploaded in parallel parts,
// must reassemble into the exact archive whose checksum is reported to the Panel.
func TestS3BackupUploadsAnArchiveThatReassemblesToTheReportedChecksum(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	serverDir := filepath.Join(root, "server")
	for _, dir := range []string{backupDir, serverDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
			Backups:         config.Backups{UploadConcurrency: 4},
		},
	})

	// Random bytes do not compress, so the archive spans many parts.
	content := make([]byte, 64*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "world.dat"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	s3 := newFakeS3(t)
	backup := NewS3(&uploadURLClient{s3: s3, partSize: 4096}, "11111111-1111-1111-1111-111111111111", "")
	details, err := backup.Generate(context.Background(), fsys, "")
	if err != nil {
		t.Fatalf("backup failed: %v", err)
	}

	if len(details.Parts) < 10 {
		t.Fatalf("expected the archive to span at least 10 parts, got %d", len(details.Parts))
	}
	var uploaded bytes.Buffer
	for i, part := range details.Parts {
		if part.PartNumber != i+1 || part.ETag != fmt.Sprintf(`"etag-%d"`, i+1) {
			t.Fatalf("expected part %d with its ETag at position %d, got %+v", i+1, i, part)
		}
		uploaded.Write(s3.part(part.PartNumber))
	}

	if want := fmt.Sprintf("%x", sha1.Sum(uploaded.Bytes())); details.Checksum != want {
		t.Fatalf("expected the reported checksum %s to match the uploaded archive %s", details.Checksum, want)
	}
	if details.Size != int64(uploaded.Len()) {
		t.Fatalf("expected the reported size %d to match the uploaded archive %d", details.Size, uploaded.Len())
	}

	// The uploaded archive must also restore the server's file intact.
	gz, err := gzip.NewReader(&uploaded)
	if err != nil {
		t.Fatalf("uploaded archive is not gzip: %v", err)
	}
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if err != nil {
			t.Fatalf("world.dat not found in uploaded archive: %v", err)
		}
		if header.Name != "world.dat" {
			continue
		}
		restored, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restored, content) {
			t.Fatal("world.dat in the uploaded archive does not match the server's file")
		}
		return
	}
}
