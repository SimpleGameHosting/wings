package router

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/router/downloader"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/filesystem"
)

// getServerFileContents returns the contents of a file on the server.
func getServerFileContents(c *gin.Context) {
	s := middleware.ExtractServer(c)
	p := strings.TrimLeft(c.Query("file"), "/")
	f, st, err := s.Filesystem().File(p)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer f.Close()
	// Don't allow a named pipe to be opened.
	//
	// @see https://github.com/pterodactyl/panel/issues/4059
	if st.Mode()&os.ModeNamedPipe != 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Cannot open files of this type.",
		})
		return
	}

	c.Header("X-Mime-Type", st.Mimetype)
	c.Header("Content-Length", strconv.Itoa(int(st.Size())))
	// If a download parameter is included in the URL go ahead and attach the necessary headers
	// so that the file can be downloaded.
	if c.Query("download") != "" {
		c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(st.Name()))
		c.Header("Content-Type", "application/octet-stream")
	}
	defer c.Writer.Flush()
	// If you don't do a limited reader here you will trigger a panic on write when
	// a different server process writes content to the file after you've already
	// determined the file size. This could lead to some weird content output but
	// it would technically be accurate based on the content at the time of the request.
	//
	// "http: wrote more than the declared Content-Length"
	//
	// @see https://github.com/pterodactyl/panel/issues/3131
	r := io.LimitReader(f, st.Size())
	if _, err = bufio.NewReader(r).WriteTo(c.Writer); err != nil {
		// Pretty sure this will unleash chaos on the response, but its a risk we can
		// take since a panic will at least be recovered and this should be incredibly
		// rare?
		middleware.CaptureAndAbort(c, err)
		return
	}
}

// Returns the contents of a directory for a server.
func getServerListDirectory(c *gin.Context) {
	s := ExtractServer(c)
	dir := c.Query("directory")
	if stats, err := s.Filesystem().ListDirectory(dir); err != nil {
		middleware.CaptureAndAbort(c, err)
	} else {
		c.JSON(http.StatusOK, stats)
	}
}

type renameFile struct {
	To   string `json:"to"`
	From string `json:"from"`
}

// Renames (or moves) files for a server.
func putServerRenameFiles(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Root  string       `json:"root"`
		Files []renameFile `json:"files"`
	}
	// BindJSON sends 400 if the request fails, all we need to do is return
	if err := c.BindJSON(&data); err != nil {
		return
	}

	if len(data.Files) == 0 {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
			"error": "No files to move or rename were provided.",
		})
		return
	}

	g, ctx := errgroup.WithContext(c.Request.Context())
	// Loop over the array of files passed in and perform the move or rename action against each.
	for _, p := range data.Files {
		pf := path.Join(data.Root, p.From)
		pt := path.Join(data.Root, p.To)

		g.Go(func() error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				fs := s.Filesystem()
				// Ignore renames on a file that is on the denylist (both as the rename from or
				// the rename to value).
				if err := fs.IsIgnored(pf, pt); err != nil {
					return err
				}
				if err := fs.Rename(pf, pt); err != nil {
					// Return nil if the error is an is not exists.
					if errors.Is(err, os.ErrNotExist) {
						s.Log().WithField("error", err).
							WithField("from_path", pf).
							WithField("to_path", pt).
							Warn("failed to rename: source or target does not exist")
						return nil
					}
					return err
				}
				return nil
			}
		})
	}

	if err := g.Wait(); err != nil {
		if errors.Is(err, os.ErrExist) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Cannot move or rename file, destination already exists.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// Copies a server file.
func postServerCopyFile(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Location string `json:"location"`
	}
	// BindJSON sends 400 if the request fails, all we need to do is return
	if err := c.BindJSON(&data); err != nil {
		return
	}

	if err := s.Filesystem().IsIgnored(data.Location); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if err := s.Filesystem().Copy(data.Location); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// Deletes files from a server.
func postServerDeleteFiles(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Root  string   `json:"root"`
		Files []string `json:"files"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if len(data.Files) == 0 {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
			"error": "No files were specified for deletion.",
		})
		return
	}

	g, ctx := errgroup.WithContext(context.Background())

	// Loop over the array of files passed in and delete them. If any of the file deletions
	// fail just abort the process entirely.
	for _, p := range data.Files {
		pi := path.Join(data.Root, p)

		g.Go(func() error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return s.Filesystem().Delete(pi)
			}
		})
	}

	if err := g.Wait(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// Writes the contents of the request to a file on a server.
func postServerWriteFile(c *gin.Context) {
	s := ExtractServer(c)

	f := c.Query("file")
	f = "/" + strings.TrimLeft(f, "/")

	if err := s.Filesystem().IsIgnored(f); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// A content length of -1 means the actual length is unknown.
	if c.Request.ContentLength == -1 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Missing Content-Length",
		})
		return
	}

	if err := s.Filesystem().Write(f, c.Request.Body, c.Request.ContentLength, 0o644); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeIsDirectory) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Cannot write file, name conflicts with an existing directory by the same name.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// Returns all of the currently in-progress file downloads and their current download
// progress. The progress is also pushed out via a websocket event allowing you to just
// call this once to get current downloads, and then listen to targeted websocket events
// with the current progress for everything.
func getServerPullingFiles(c *gin.Context) {
	s := ExtractServer(c)
	c.JSON(http.StatusOK, gin.H{
		"downloads": downloader.ByServer(s.ID()),
	})
}

// Writes the contents of the remote URL to a file on a server.
func postServerPullRemoteFile(c *gin.Context) {
	s := ExtractServer(c)
	var data struct {
		// Deprecated
		Directory  string `binding:"required_without=RootPath,omitempty" json:"directory"`
		RootPath   string `binding:"required_without=Directory,omitempty" json:"root"`
		URL        string `binding:"required" json:"url"`
		FileName   string `json:"file_name"`
		UseHeader  bool   `json:"use_header"`
		Foreground bool   `json:"foreground"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	// Handle the deprecated Directory field in the struct until it is removed.
	if data.Directory != "" && data.RootPath == "" {
		data.RootPath = data.Directory
	}

	u, err := url.Parse(data.URL)
	if err != nil {
		if e, ok := err.(*url.Error); ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "An error occurred while parsing that URL: " + e.Err.Error(),
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	if err := s.Filesystem().HasSpaceErr(true); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	// Do not allow more than three simultaneous remote file downloads at one time.
	if len(downloader.ByServer(s.ID())) >= 3 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "This server has reached its limit of 3 simultaneous remote file downloads at once. Please wait for one to complete before trying again.",
		})
		return
	}

	dl := downloader.New(s, downloader.DownloadRequest{
		Directory: data.RootPath,
		URL:       u,
		FileName:  data.FileName,
		UseHeader: data.UseHeader,
	})

	download := func() error {
		s.Log().WithField("download_id", dl.Identifier).WithField("url", u.Redacted()).Info("starting pull of remote file to disk")
		if err := dl.Execute(); err != nil {
			if !downloader.IsDownloadError(err) {
				s.Log().WithField("download_id", dl.Identifier).WithField("error", err).Error("failed to pull remote file")
			}
			return err
		}
		s.Log().WithField("download_id", dl.Identifier).Info("completed pull of remote file")
		return nil
	}

	if !data.Foreground {
		go func() {
			_ = download()
		}()
		c.JSON(http.StatusAccepted, gin.H{
			"identifier": dl.Identifier,
		})
		return
	}

	if err := download(); err != nil {
		if downloader.IsDownloadError(err) {
			var message = "The URL or IP address provided could not be resolved to a valid destination."
			if errors.Is(err, downloader.ErrDownloadFailed) {
				s.Log().WithField("identifier", dl.Identifier).WithField("error", err).Warn("failed to download remote file")

				message = "An error was encountered while trying to download this file. Please try again later."
			}

			c.JSON(http.StatusBadRequest, gin.H{
				"identifier": dl.Identifier,
				"message":    message,
			})

			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	st, err := s.Filesystem().Stat(dl.Path())
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, &st)
}

// Stops a remote file download if it exists and belongs to this server.
func deleteServerPullRemoteFile(c *gin.Context) {
	s := ExtractServer(c)
	if dl := downloader.ByID(c.Param("download")); dl != nil && dl.BelongsTo(s) {
		dl.Cancel()
	}
	c.Status(http.StatusNoContent)
}

// Create a directory on a server.
func postServerCreateDirectory(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	// BindJSON sends 400 if the request fails, all we need to do is return
	if err := c.BindJSON(&data); err != nil {
		return
	}

	if err := s.Filesystem().CreateDirectory(data.Name, data.Path); err != nil {
		if err.Error() == "not a directory" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Part of the path being created is not a directory (ENOTDIR).",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

func postServerCompressFiles(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		RootPath string   `json:"root"`
		Files    []string `json:"files"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if len(data.Files) == 0 {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
			"error": "No files were passed through to be compressed.",
		})
		return
	}

	if !s.Filesystem().HasSpaceAvailable(true) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "This server does not have enough available disk space to generate a compressed archive.",
		})
		return
	}

	f, err := s.Filesystem().CompressFiles(data.RootPath, data.Files)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, &filesystem.Stat{
		FileInfo: f,
		Mimetype: "application/tar+gzip",
	})
}

// postServerDecompressFiles validates the archive quickly and then unpacks it
// in the background: multi-gigabyte archives routinely outlive proxy timeouts,
// which made users retry and double-extract, so the request must not block on
// the extraction itself.
func postServerDecompressFiles(c *gin.Context) {
	var data struct {
		RootPath string `json:"root"`
		File     string `json:"file"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	s := middleware.ExtractServer(c)
	lg := middleware.ExtractLogger(c).WithFields(log.Fields{"root_path": data.RootPath, "file": data.File})

	// Reject a missing or unrecognized archive synchronously so the caller
	// still gets an immediate, accurate error. This only reads the archive
	// header; the expensive space walk and extraction run in the background...
	if err := s.Filesystem().CanDecompressFile(c.Request.Context(), data.RootPath, data.File); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeUnknownArchive) {
			lg.WithField("error", err).Warn("failed to decompress file: unknown archive format")
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The archive provided is in a format Wings does not understand."})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Refuse to start a second extraction of the same archive: the instant
	// 202 makes double-submits cheap, and competing extractions would race
	// each other writing into the same tree...
	release, ok := s.Filesystem().TryStartDecompression(data.RootPath, data.File)
	if !ok {
		lg.Info("rejected decompression request: this archive is already being decompressed")
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "This archive is already being decompressed, wait for it to finish.",
		})
		return
	}

	lg.Info("starting background file decompression")
	go runBackgroundDecompression(s, data.RootPath, data.File, release)

	c.Status(http.StatusAccepted)
}

// runBackgroundDecompression performs the disk-space walk and the extraction
// for postServerDecompressFiles outside the request lifecycle, releasing the
// archive's single-flight reservation when it finishes.
func runBackgroundDecompression(s *server.Server, rootPath string, file string, release func()) {
	defer release()

	decompressRecoveryGuard(s, file, func() {
		s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Decompressing archive %s, this may take a while...", file))
		backgroundLog := s.Log().WithFields(log.Fields{"root_path": rootPath, "file": file})

		// The server context ties the walk and the extraction to the server's
		// lifetime rather than the already-completed HTTP request...
		if err := s.Filesystem().SpaceAvailableForDecompression(s.Context(), rootPath, file); err != nil {
			s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Unable to decompress %s: not enough disk space is available.", file))
			backgroundLog.WithField("error", err).Warn("failed to decompress file in background: insufficient space")
			return
		}

		if err := s.Filesystem().DecompressFile(s.Context(), rootPath, file); err != nil {
			// If the file is busy the user needs to stop the running server
			// process before this archive can overwrite its files.
			if strings.Contains(err.Error(), "text file busy") {
				s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Unable to decompress %s: one or more of its files are in use by the running server process. Stop the server and try again.", file))
				backgroundLog.WithField("error", errors.WithStackIf(err)).Warn("failed to decompress file in background: text file busy")
				return
			}
			s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Decompressing %s failed; contact support if this keeps happening.", file))
			backgroundLog.WithField("error", errors.WithStackIf(err)).Error("failed to decompress file in background")
			return
		}

		s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Finished decompressing %s.", file))
	})
}

// decompressRecoveryGuard runs work and converts any panic into a console
// message and an error log. The archive bytes driving the parser are tenant
// controlled, and a panic in this background goroutine would otherwise crash
// the whole daemon instead of failing one request.
func decompressRecoveryGuard(s *server.Server, file string, work func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Decompressing %s failed; contact support if this keeps happening.", file))
			s.Log().WithFields(log.Fields{"file": file, "panic": recovered}).Error("recovered from panic during background decompression")
		}
	}()

	work()
}

type chmodFile struct {
	File string `json:"file"`
	Mode string `json:"mode"`
}

var errInvalidFileMode = errors.New("invalid file mode")

func postServerChmodFile(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		Root  string      `json:"root"`
		Files []chmodFile `json:"files"`
	}

	if err := c.BindJSON(&data); err != nil {
		log.Debug(err.Error())
		return
	}

	if len(data.Files) == 0 {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
			"error": "No files to chmod were provided.",
		})
		return
	}

	g, ctx := errgroup.WithContext(context.Background())

	// Loop over the array of files passed in and perform the move or rename action against each.
	for _, p := range data.Files {
		g.Go(func() error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				mode, err := strconv.ParseUint(p.Mode, 8, 32)
				if err != nil {
					return errInvalidFileMode
				}

				if err := s.Filesystem().Chmod(path.Join(data.Root, p.File), os.FileMode(mode)); err != nil {
					// Return nil if the error is an is not exists.
					// NOTE: os.IsNotExist() does not work if the error is wrapped.
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}

					return err
				}

				return nil
			}
		})
	}

	if err := g.Wait(); err != nil {
		if errors.Is(err, errInvalidFileMode) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Invalid file mode.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

func postServerUploadFiles(c *gin.Context) {
	if c.GetHeader(uploadCompleteHeader) != "" {
		postServerCreateResumableUpload(c)
		return
	}

	s, token, ok := authenticateServerUpload(c, true, true)
	if !ok {
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Failed to get multipart form data from request.",
		})
		return
	}

	headers, ok := form.File["files"]
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "No files were found on the request body.",
		})
		return
	}

	directory := c.Query("directory")

	maxFileSize := config.Get().Api.UploadLimit
	maxFileSizeBytes := maxFileSize * 1024 * 1024
	var totalSize int64
	for _, header := range headers {
		if header.Size > maxFileSizeBytes {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "File " + header.Filename + " is larger than the maximum file upload size of " + strconv.FormatInt(maxFileSize, 10) + " MB.",
			})
			return
		}
		totalSize += header.Size
	}

	for _, header := range headers {
		// We run this in a different method so I can use defer without any of
		// the consequences caused by calling it in a loop.
		if err := handleFileUpload(filepath.Join(directory, header.Filename), s, header); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		} else {
			s.SaveActivity(s.NewRequestActivity(token.UserUuid, c.ClientIP()), server.ActivityFileUploaded, models.ActivityMeta{
				"file":      header.Filename,
				"directory": filepath.Clean(directory),
			})
		}
	}
}

func handleFileUpload(p string, s *server.Server, header *multipart.FileHeader) error {
	file, err := header.Open()
	if err != nil {
		return err
	}
	defer file.Close()

	if err := s.Filesystem().IsIgnored(p); err != nil {
		return err
	}

	if err := s.Filesystem().Write(p, file, header.Size, 0o644); err != nil {
		return err
	}
	return nil
}
