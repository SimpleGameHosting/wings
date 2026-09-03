package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/modpackinstall"
	"github.com/pterodactyl/wings/router/middleware"
)

// postServerModpackInstall admits one native modpack or version install job
// for a server. Everything that could conflict with the attempt, another
// exclusive operation already running on this server, or the node being out
// of install capacity, is claimed synchronously here before the 202 is
// written, so a duplicate, a racing request, or an over-quota attempt is
// always answered honestly instead of failing invisibly inside a background
// goroutine later on. The order below is validate, admit, reserve: a
// malformed payload was never admitted and so can never be a repeat, which
// is why it is rejected first; admission then decides in one critical
// section whether this is an exact repeat of an attempt this server has
// already admitted, whether still running or the last to finish, answered
// again so a caller retrying a lost 202 is never told the server is busy
// and never starts the same install twice, or a fresh attempt that claims
// the server's exclusive operation and records its identity together;
// finally the node-wide install slot is reserved, unwinding that claim
// again if this last step fails.
func postServerModpackInstall(c *gin.Context) {
	s := middleware.ExtractServer(c)
	manager := middleware.ExtractManager(c)

	var req modpackinstall.Request
	if err := c.BindJSON(&req); err != nil {
		return
	}

	if err := req.Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":      err.Error(),
			"request_id": c.GetString("request_id"),
		})
		return
	}

	// Admission is one critical section: a repeat of an attempt this server
	// already admitted is answered again without a second job, otherwise
	// the exclusive operation is claimed and the id recorded together, so
	// two simultaneous retries can never disagree...
	repeat, err := s.AdmitModpackInstall(req.InstallID)
	if repeat {
		c.JSON(http.StatusAccepted, gin.H{"install_id": req.InstallID})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":      operationConflictMessage(s.CurrentOperation()),
			"request_id": c.GetString("request_id"),
		})
		return
	}

	// The operation claim above must never outlive a failed slot
	// reservation, since nothing else on this request path would ever
	// release it otherwise. Releasing through the fence keeps the id and
	// the reservation consistent, and deliberately does not remember the
	// id as finished, since nothing ran...
	release, ok := manager.TryReserveModpackInstallSlot()
	if !ok {
		s.AbandonModpackInstallClaim(req.InstallID)
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":      "this node is running its maximum number of concurrent installs, try again shortly",
			"request_id": c.GetString("request_id"),
		})
		return
	}

	go s.RunModpackInstall(req, release)

	c.JSON(http.StatusAccepted, gin.H{"install_id": req.InstallID})
}
