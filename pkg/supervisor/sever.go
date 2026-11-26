package supervisor

// SeverConnections closes accepted control sockets and does not stop the
// VM. The agent's death watch sees the same peer close a SIGKILL of the
// supervisor process produces, and marks the slot lost.
func (s *Server) SeverConnections() {
	if s == nil {
		return
	}
	s.closeActive()
}
