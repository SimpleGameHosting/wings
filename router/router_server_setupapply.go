package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/setupapply"
	"github.com/pterodactyl/wings/router/middleware"
)

// postServerSetupApply admits one native guided setup apply job for a
// server. The payload is validated first, then admission happens in one
// critical section: a repeat of an attempt this server already admitted is
// answered 202 again without a second job, otherwise the exclusive
// operation is claimed and the setup_id recorded together, or the request
// is refused with 409 naming the operation that holds the server. There is
// no node-wide slot for this job; it is seconds of local file work.
func postServerSetupApply(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var req setupapply.Request
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

	repeat, err := s.AdmitSetupApply(req.SetupID)
	if repeat {
		c.JSON(http.StatusAccepted, gin.H{"setup_id": req.SetupID})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":      operationConflictMessage(s.CurrentOperation()),
			"request_id": c.GetString("request_id"),
		})
		return
	}

	go s.RunSetupApply(req)

	c.JSON(http.StatusAccepted, gin.H{"setup_id": req.SetupID})
}
