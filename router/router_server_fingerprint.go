package router

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/router/middleware"
)

// fingerprintDeadline bounds a single fingerprint walk so that a pathological
// file tree fails the request instead of holding a panel worker indefinitely.
const fingerprintDeadline = 60 * time.Second

// postServerFingerprint computes the content fingerprint of a server's files,
// honouring the same ignore lines the panel sends when requesting a backup.
func postServerFingerprint(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var data struct {
		Ignore string `json:"ignore"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), fingerprintDeadline)
	defer cancel()

	result, err := s.Filesystem().Fingerprint(ctx, data.Ignore)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			middleware.ExtractLogger(c).WithField("deadline", fingerprintDeadline.String()).Warn("router: server fingerprint exceeded its deadline")
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, gin.H{
				"error":      "The server fingerprint could not be computed within the deadline.",
				"request_id": c.GetString("request_id"),
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}
