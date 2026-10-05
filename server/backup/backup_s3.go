package backup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/cenkalti/backoff/v4"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type S3Backup struct {
	Backup
}

var _ BackupInterface = (*S3Backup)(nil)

func NewS3(client remote.Client, uuid string, ignore string) *S3Backup {
	return &S3Backup{
		Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: S3BackupAdapter,
		},
	}
}

// Remove removes a backup from the system.
func (s *S3Backup) Remove() error {
	if err := s.validateIdentifier(); err != nil {
		return err
	}
	return os.Remove(s.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (s *S3Backup) WithLogContext(c map[string]interface{}) {
	s.logContext = c
}

// Generate creates a new backup on the disk, moves it into the S3 bucket via
// the provided presigned URL, and then deletes the backup from the disk.
func (s *S3Backup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := s.validateIdentifier(); err != nil {
		return nil, err
	}
	defer s.Remove()

	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	s.log().WithField("path", s.Path()).Info("creating backup for server")
	if err := a.Create(ctx, s.Path()); err != nil {
		return nil, err
	}
	s.log().Info("created backup successfully")

	archive, err := os.Open(s.Path())
	if err != nil {
		return nil, errors.Wrap(err, "backup: could not read archive from disk")
	}
	defer archive.Close()

	parts, err := s.generateRemoteRequest(ctx, archive)
	if err != nil {
		return nil, err
	}
	ad, err := s.Details(ctx, parts)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details after upload")
	}
	return ad, nil
}

// Restore will read from the provided reader assuming that it is a gzipped
// tar reader. When a file is encountered in the archive the callback function
// will be triggered. If the callback returns an error the entire process is
// stopped, otherwise this function will run until all files have been written.
//
// This restoration uses a workerpool to use up to the number of CPUs available
// on the machine when writing files to the disk.
func (s *S3Backup) Restore(ctx context.Context, r io.Reader, callback RestoreCallback) error {
	reader := r
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(r, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}
	if err := format.Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		return callback(f.NameInArchive, f, r)
	}); err != nil {
		return err
	}
	return nil
}

// generateRemoteRequest asks the Panel for presigned part URLs and uploads the
// archive to them within this node's shared upload limits.
func (s *S3Backup) generateRemoteRequest(ctx context.Context, archive io.ReaderAt) ([]remote.BackupPart, error) {
	s.log().Debug("attempting to get size of backup...")
	size, err := s.Backup.Size()
	if err != nil {
		return nil, err
	}
	s.log().WithField("size", size).Debug("got size of backup")

	s.log().Debug("attempting to get S3 upload urls from Panel...")
	urls, err := s.client.GetBackupRemoteUploadURLs(context.Background(), s.Backup.Uuid, size)
	if err != nil {
		return nil, err
	}
	s.log().Debug("got S3 upload urls from the Panel")
	s.log().WithField("parts", len(urls.Parts)).Info("attempting to upload backup to s3 endpoint...")

	// Each attempt at a part gets two hours. The node's speed limit is shared by at most
	// UploadConcurrency parts, so even a 5 GiB part has a wide margin at 80 MiB/s.
	client := &http.Client{Timeout: time.Hour * 2}

	parts, err := uploadParts(ctx, client, nodeUploadLimits(), archive, urls, size, s.log())
	if err != nil {
		return nil, err
	}
	s.log().WithField("parts", len(urls.Parts)).Info("backup has been successfully uploaded")

	return parts, nil
}

// uploadLimits bound the S3 uploads of every backup on this node together, so that
// several backups running at once share the port instead of each claiming all of it.
type uploadLimits struct {
	// slots holds one token per part that is currently uploading.
	slots chan struct{}

	// bucket caps the combined upload speed, or is nil when the speed is unlimited.
	bucket *ratelimit.Bucket
}

var (
	nodeUploadLimitsOnce sync.Once
	nodeUploadLimitsInst *uploadLimits
)

// nodeUploadLimits returns the limits shared by every backup on this node, built from
// the configuration the first time a backup uploads.
func nodeUploadLimits() *uploadLimits {
	nodeUploadLimitsOnce.Do(func() {
		backups := config.Get().System.Backups
		nodeUploadLimitsInst = newUploadLimits(backups.UploadConcurrency, backups.UploadLimit)
	})

	return nodeUploadLimitsInst
}

// newUploadLimits allows concurrency parts to upload at once, at no more than
// limitMiB MiB/s combined. A concurrency below 1 uploads one part at a time, and a
// limit below 1 leaves the speed unlimited.
func newUploadLimits(concurrency int, limitMiB int) *uploadLimits {
	if concurrency < 1 {
		concurrency = 1
	}

	limits := &uploadLimits{slots: make(chan struct{}, concurrency)}
	if limitMiB > 0 {
		rate := float64(limitMiB) * 1024 * 1024
		limits.bucket = ratelimit.NewBucketWithRate(rate, int64(rate))
	}

	return limits
}

// acquire waits for a free upload slot and reports whether it got one. It never
// holds a slot when ctx is already done, because select picks at random between a
// free slot and a cancelled context, and a slot taken then would never be returned.
func (l *uploadLimits) acquire(ctx context.Context) bool {
	select {
	case l.slots <- struct{}{}:
		if ctx.Err() != nil {
			<-l.slots
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// uploadParts uploads every part of the archive to its presigned URL, running as many
// parts at once as the node has free slots. The first part that fails for good cancels
// the others, since the backup cannot be completed without it.
//
// The returned parts are in part number order, as CompleteMultipartUpload expects.
func uploadParts(
	ctx context.Context,
	client *http.Client,
	limits *uploadLimits,
	archive io.ReaderAt,
	upload remote.BackupRemoteUploadResponse,
	size int64,
	logger *log.Entry,
) ([]remote.BackupPart, error) {
	parts := make([]remote.BackupPart, len(upload.Parts))
	group, groupCtx := errgroup.WithContext(ctx)

	for i, url := range upload.Parts {
		number := i + 1
		offset := int64(i) * upload.PartSize
		length := upload.PartSize
		if number == len(upload.Parts) {
			// The last part holds whatever remains and has no minimum size.
			length = size - offset
		}

		// Parts start in order as slots free up, so a failure stops the queue here.
		if !limits.acquire(groupCtx) {
			break
		}

		group.Go(func() error {
			defer func() { <-limits.slots }()

			etag, err := uploadPart(groupCtx, client, limits, url, io.NewSectionReader(archive, offset, length))
			if err != nil {
				logger.WithField("part_id", number).WithError(err).Warn("failed to upload part")
				return err
			}
			parts[i] = remote.BackupPart{ETag: etag, PartNumber: number}
			logger.WithField("part_id", number).Info("successfully uploaded backup part")

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}
	// The caller's context can end while parts are still queued for a slot, which
	// stops the queue without any part reporting an error.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return parts, nil
}

// uploadBackoff returns an exponential backoff that also stops when ctx is cancelled.
func uploadBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 2
	b.MaxElapsedTime = time.Minute

	return backoff.WithContext(b, ctx)
}

// uploadPart uploads one part to its presigned URL and returns the ETag that S3
// assigned to it. A 5xx response is retried with an exponential backoff, and every
// attempt sends the whole part again from its first byte.
func uploadPart(ctx context.Context, client *http.Client, limits *uploadLimits, url string, part *io.SectionReader) (string, error) {
	var etag string
	err := backoff.Retry(func() error {
		if _, err := part.Seek(0, io.SeekStart); err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not rewind archive part"))
		}
		var body io.Reader = part
		if limits.bucket != nil {
			body = ratelimit.Reader(part, limits.bucket)
		}

		r, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.NopCloser(body))
		if err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not create request for S3"))
		}
		r.ContentLength = part.Size()
		r.Header.Add("Content-Type", "application/x-gzip")

		res, err := client.Do(r)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return backoff.Permanent(err)
			}
			// Don't use a permanent error here, if there is a temporary resolution error with
			// the URL due to DNS issues we want to keep re-trying.
			return errors.Wrap(err, "backup: S3 HTTP request failed")
		}
		_ = res.Body.Close()

		if res.StatusCode != http.StatusOK {
			err := errors.New(fmt.Sprintf("backup: failed to put S3 object: [HTTP/%d] %s", res.StatusCode, res.Status))
			// Only attempt a backoff retry if this error is because of a 5xx error from
			// the S3 endpoint. Any 4xx error should be treated as an error that a retry
			// would not fix.
			if res.StatusCode >= http.StatusInternalServerError {
				return err
			}
			return backoff.Permanent(err)
		}

		// Get the ETag from the uploaded part, this should be sent with the
		// CompleteMultipartUpload request.
		etag = res.Header.Get("ETag")

		return nil
	}, uploadBackoff(ctx))
	if err != nil {
		if v, ok := err.(*backoff.PermanentError); ok {
			return "", v.Unwrap()
		}
		return "", err
	}

	return etag, nil
}
