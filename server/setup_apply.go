package server

// AdmitSetupApply admits one native setup apply attempt in a single
// critical section: a repeat of the running or most recently finished id
// reports repeat true without claiming, otherwise the install reservation
// is claimed and the id recorded together, or ErrOperationInProgress is
// returned when another operation holds the server. The install kind is
// reused because it already cancels SFTP sessions and blocks power actions.
func (s *Server) AdmitSetupApply(id string) (bool, error) {
	return s.admitFenced(&s.operation.setup, OperationInstall, id)
}

// ActiveSetupApplyID reports the setup_id of the apply currently running
// against this server, or the empty string.
func (s *Server) ActiveSetupApplyID() string {
	s.operation.mu.Lock()
	defer s.operation.mu.Unlock()
	return s.operation.setup.active
}

// releaseSetupApplyClaim retires a finished attempt together with the
// reservation.
func (s *Server) releaseSetupApplyClaim(setupID string) {
	s.releaseFenced(&s.operation.setup, OperationInstall, setupID)
}
