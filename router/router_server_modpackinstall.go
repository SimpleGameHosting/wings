package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/modpackinstall"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
)

// postServerModpackInstall admits one native modpack or version install job
// for a server. Everything that could conflict with the attempt, another
// exclusive operation already running on this server, or the node being out
// of install capacity, is claimed synchronously here before the 202 is
// written, so a duplicate, a racing request, or an over-quota attempt is
// always answered honestly instead of failing invisibly inside a background
// goroutine later on. The claim order below is spec-mandated: an exact
// repeat of the attempt already running is answered first, so a caller
// retrying a lost 202 is never told the server is busy; only after that is
// the payload validated, the server's exclusive operation claimed, and
// finally the node-wide install slot reserved, unwinding the operation
// claim again if that last step fails.
func postServerModpackInstall(c *gin.Context) {
	s := middleware.ExtractServer(c)
	manager := middleware.ExtractManager(c)

	var req modpackinstall.Request
	if err := c.BindJSON(&req); err != nil {
		return
	}

	// An exact repeat of the install currently running means our earlier
	// 202 was lost in transit somewhere; answer it again without starting a
	// second job...
	if active := s.ActiveModpackInstallID(); active != "" && active == req.InstallID {
		c.JSON(http.StatusAccepted, gin.H{"install_id": req.InstallID})
		return
	}

	if err := req.Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":      err.Error(),
			"request_id": c.GetString("request_id"),
		})
		return
	}

	// Claim exclusive ownership of the server before touching the node-wide
	// slot, so a server already busy with a transfer, restore, power
	// action, or another install is rejected without taking a slot away
	// from a request that could actually proceed...
	if err := s.TryBeginOperation(server.OperationInstall); err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":      operationConflictMessage(s.CurrentOperation()),
			"request_id": c.GetString("request_id"),
		})
		return
	}

	// The operation claim above must never outlive a failed slot
	// reservation, since nothing else on this request path would ever
	// release it otherwise...
	release, ok := manager.TryReserveModpackInstallSlot()
	if !ok {
		s.EndOperation(server.OperationInstall)
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":      "this node is running its maximum number of concurrent installs, try again shortly",
			"request_id": c.GetString("request_id"),
		})
		return
	}

	go s.RunModpackInstall(req, release)

	c.JSON(http.StatusAccepted, gin.H{"install_id": req.InstallID})
}
