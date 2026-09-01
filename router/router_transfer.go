package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/installer"
	"github.com/pterodactyl/wings/server/transfer"
)

// postTransfers .
func postTransfers(c *gin.Context) {
	auth := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(auth) != 2 || auth[0] != "Bearer" {
		c.Header("WWW-Authenticate", "Bearer")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "The required authorization headers were not present in the request.",
		})
		return
	}

	token := tokens.TransferPayload{}
	if err := tokens.ParseToken([]byte(auth[1]), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !token.HasScope(tokens.ServerTransfer) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		return
	}

	manager := middleware.ExtractManager(c)
	u, err := uuid.Parse(token.Subject)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Only one request may drive a transfer for a server. A duplicate POST
	// would share the in-flight transfer, and its failure cleanup would
	// delete the files the original request is still extracting. The atomic
	// reservation also covers the window while the installer talks to the
	// panel and, because its release below is registered before the finalize
	// defer, it is held until cleanup has completely settled.
	if !transfer.Incoming().TryReserve(u.String()) {
		log.WithField("server", u.String()).Info("rejected transfer request: a transfer for this server is already in progress")
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "A transfer for this server is already in progress.",
		})
		return
	}
	defer transfer.Incoming().Release(u.String())

	// A server that already exists on this node can never be the destination
	// of a transfer. Accepting a stray or replayed request would end with the
	// failure cleanup deleting the live server's data directory.
	if _, exists := manager.Get(u.String()); exists {
		log.WithField("server", u.String()).Info("rejected transfer request: the server already exists on this node")
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "The server this transfer is for already exists on this node.",
		})
		return
	}

	// TODO: should this use the request context?
	trnsfr := transfer.New(c, nil)

	ctx, cancel := context.WithCancel(trnsfr.Context())
	defer cancel()

	i, err := installer.New(ctx, manager, installer.ServerDetails{
		UUID:              u.String(),
		StartOnCompletion: false,
	})
	if err != nil {
		// trnsfr.Server is only assigned once the installer succeeds, so
		// this failure report must use the UUID parsed from the token.
		if err := manager.Client().SetTransferStatus(context.Background(), u.String(), false); err != nil {
			trnsfr.Log().WithField("status", false).WithError(err).Error("failed to set transfer status")
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	if err := i.Server().TryBeginOperation(server.OperationTransfer); err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": operationConflictMessage(i.Server().CurrentOperation()),
		})
		return
	}
	manager.Add(i.Server())

	// We add the transfer to the list of transfers once we have a server instance to use.
	trnsfr.Server = i.Server()
	transfer.Incoming().Add(trnsfr)

	// Any errors past this point (until the transfer is complete) will abort
	// the transfer.

	successful := false
	defer func() {
		finalizeIncomingTransfer(manager, trnsfr, successful)
	}()

	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		trnsfr.Log().Debug("failed to parse content type header")
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		trnsfr.Log().Debug("invalid content type")
		middleware.CaptureAndAbort(c, fmt.Errorf("invalid content type \"%s\", expected \"multipart/form-data\"", mediaType))
		return
	}

	// Used to calculate the hash of the file as it is being uploaded.
	h := sha256.New()

	// Used to read the file and checksum from the request body.
	mr := multipart.NewReader(c.Request.Body, params["boundary"])

	// Loop through the parts of the request body and process them.
	var (
		hasArchive       bool
		hasChecksum      bool
		checksumVerified bool
	)
out:
	for {
		select {
		case <-ctx.Done():
			break out
		default:
			p, err := mr.NextPart()
			if err == io.EOF {
				break out
			}
			if err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}

			name := p.FormName()
			switch name {
			case "archive":
				trnsfr.Log().Debug("received archive")

				if err := trnsfr.Server.EnsureDataDirectoryExists(); err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}

				tee := io.TeeReader(p, h)
				if err := trnsfr.Server.Filesystem().ExtractStreamUnsafe(ctx, "/", tee); err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}

				hasArchive = true
			case "checksum":
				trnsfr.Log().Debug("received checksum")

				if !hasArchive {
					middleware.CaptureAndAbort(c, errors.New("archive must be sent before the checksum"))
					return
				}

				hasChecksum = true

				v, err := io.ReadAll(p)
				if err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}

				expected := make([]byte, hex.DecodedLen(len(v)))
				n, err := hex.Decode(expected, v)
				if err != nil {
					middleware.CaptureAndAbort(c, err)
					return
				}
				actual := h.Sum(nil)

				trnsfr.Log().WithFields(log.Fields{
					"expected": hex.EncodeToString(expected),
					"actual":   hex.EncodeToString(actual),
				}).Debug("checksums")

				if !bytes.Equal(expected[:n], actual) {
					middleware.CaptureAndAbort(c, errors.New("checksums don't match"))
					return
				}

				trnsfr.Log().Debug("checksums match")
				checksumVerified = true
			default:
				continue
			}
		}
	}

	if !hasArchive || !hasChecksum {
		middleware.CaptureAndAbort(c, errors.New("missing archive or checksum"))
		return
	}

	if !checksumVerified {
		middleware.CaptureAndAbort(c, errors.New("checksums don't match"))
		return
	}

	// Transfer is almost complete, we just want to ensure the environment is
	// configured correctly.  We might want to not fail the transfer at this
	// stage, but we will just to be safe.

	// Ensure the server environment gets configured.
	if err := trnsfr.Server.CreateEnvironment(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Changing this causes us to notify the panel about a successful transfer,
	// rather than failing the transfer like we do by default.
	successful = true

	// The rest of the logic for ensuring the server is unlocked and everything
	// is handled in the deferred function above.
	trnsfr.Log().Debug("done!")
}

// finalizeIncomingTransfer settles an incoming transfer once the request body
// has been fully processed. A failed transfer must always drop the server and
// its extracted files from this node, and the panel is notified only after
// that cleanup so a retried transfer cannot race a directory that is still
// being deleted.
func finalizeIncomingTransfer(manager *server.Manager, trnsfr *transfer.Transfer, successful bool) {
	// Remove the transfer from the list of incoming transfers.
	transfer.Incoming().Remove(trnsfr)

	status := "success"
	if !successful {
		status = "failure"

		// First, drop the half-transferred server and its extracted files so a
		// failed transfer cannot leak disk space on this node...
		manager.Remove(func(match *server.Server) bool {
			return match.ID() == trnsfr.Server.ID()
		})

		_ = trnsfr.Server.Filesystem().UnixFS().Close()
		if err := os.RemoveAll(trnsfr.Server.Filesystem().Path()); err != nil && !os.IsNotExist(err) {
			trnsfr.Log().WithError(err).Warn("failed to delete local server files for failed transfer")
		}
	}

	// Notify the panel after cleanup; a failed status call must not strand the
	// files or leave the server marked as transferring...
	if err := manager.Client().SetTransferStatus(context.Background(), trnsfr.Server.ID(), successful); err != nil {
		trnsfr.Log().WithField("status", status).WithError(err).Error("failed to set transfer status on panel")
	}

	trnsfr.Server.EndOperation(server.OperationTransfer)
	trnsfr.Server.Events().Publish(server.TransferStatusEvent, status)
}

// deleteTransfer cancels an incoming transfer for a server.
func deleteTransfer(c *gin.Context) {
	s := ExtractServer(c)

	if !s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr := transfer.Incoming().Get(s.ID())
	if trnsfr == nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr.Cancel()

	c.Status(http.StatusAccepted)
}
