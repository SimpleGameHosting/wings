package modpackinstall

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/server/filesystem"
)

// ProgressFunc receives download progress as bytes received so far and the
// total declared by the server. The reader throttles calls to roughly twice
// a second and always fires one final call once the transfer finishes, so
// implementations should stay cheap but need not debounce themselves. A nil
// ProgressFunc is allowed and simply disables reporting.
type ProgressFunc func(bytes, total int64)

// errRedirectRefused is the sentinel downloadClient's CheckRedirect returns
// to abort following a redirect. The standard library reports a refused
// redirect through the same *http.Client.Do error path as a genuine network
// failure, wrapped in a *url.Error, so Download unwraps this specific value
// back out to tell the two apart.
var errRedirectRefused = errors.New("modpackinstall: download failed: redirects are not permitted")

// downloadClient is shared by every Download call. A signed, one-time
// download URL must never be replayed against a host the panel did not
// name, so redirects are refused outright rather than followed; the
// transport bounds the connection phases that the caller's own context
// does not otherwise cover.
var downloadClient = &http.Client{
	// The caller's context bounds the overall transfer, so no client-wide
	// timeout is set here: a large, slow, but healthy transfer is never cut
	// short just because it runs long.
	Timeout: 0,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return errRedirectRefused
	},
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	},
}

// Download fetches the artifact at rawURL and streams it into TempArchiveName
// at the server root through the quota-accounted filesystem write path. It
// exists to turn an untrusted, panel-supplied URL into exactly one artifact
// of a known, verified size on disk, or a sanitized error: the transfer is
// rejected unless the server declares a positive Content-Length and then
// delivers exactly that many bytes, so a slow or truncated upstream can
// never leave a partial archive mistaken for a complete one. Every error
// path returns a fixed, sanitized message rather than the underlying cause,
// since rawURL may carry a signed, secret query string that must never
// reach logs or events.
func Download(ctx context.Context, fs *filesystem.Filesystem, rawURL string, progress ProgressFunc) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, errors.New("modpackinstall: download failed: invalid request")
	}

	res, err := downloadClient.Do(req)
	if err != nil {
		// A refused redirect also surfaces here, since CheckRedirect's error
		// comes back wrapped in a *url.Error alongside every other failure
		// Do can report; unwrap that specific sentinel first so the caller
		// gets the precise reason instead of a misleading connectivity
		// message...
		if errors.Is(err, errRedirectRefused) {
			return 0, errRedirectRefused
		}
		if ctx.Err() != nil {
			return 0, errors.New("modpackinstall: download failed: cancelled or timed out")
		}
		return 0, errors.New("modpackinstall: download failed: could not reach the artifact host")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return 0, errors.Errorf("modpackinstall: download failed: unexpected status %d", res.StatusCode)
	}

	total := res.ContentLength
	if total <= 0 {
		return 0, errors.New("modpackinstall: download failed: response did not declare a content length")
	}

	counter := &countingReader{r: res.Body, total: total, progress: progress}

	// fs.Write pre-checks the quota against total before touching disk, and
	// copies through an io.LimitReader capped at total, so an over-long body
	// can never write past the declared size either. Critically, that same
	// LimitReader makes a short body end the copy with a plain io.EOF rather
	// than an error: fs.Write can return nil after writing fewer bytes than
	// declared, so the exact-length check below is the only thing standing
	// between a truncated upstream and a silently short archive...
	if err := fs.Write(TempArchiveName, counter, total, 0o644); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace) {
			return 0, errors.New("modpackinstall: download failed: not enough disk space for the artifact")
		}
		return 0, errors.New("modpackinstall: download failed: could not write the artifact")
	}

	got := atomic.LoadInt64(&counter.n)
	if got != total {
		return got, errors.Errorf("modpackinstall: download failed: connection dropped mid-transfer (%d of %d bytes)", got, total)
	}

	if progress != nil {
		progress(got, total)
	}

	return got, nil
}

// countingReader wraps a response body to count bytes as they are read and
// to throttle progress callbacks, so Download can report progress without
// the filesystem write path needing to know progress reporting exists.
type countingReader struct {
	r        io.Reader
	n        int64
	total    int64
	progress ProgressFunc
	lastEmit time.Time
}

// Read delegates to the wrapped reader, then updates the running byte count
// and fires the progress callback if at least 500ms have passed since the
// last one, keeping callbacks cheap even for a very chatty reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		atomic.AddInt64(&c.n, int64(n))
		if c.progress != nil && time.Since(c.lastEmit) >= 500*time.Millisecond {
			c.lastEmit = time.Now()
			c.progress(atomic.LoadInt64(&c.n), c.total)
		}
	}
	return n, err
}
