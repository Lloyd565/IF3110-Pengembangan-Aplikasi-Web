package httpx

import "net/http"

// Owner: B. Setiap handler wajib requireAdmin.

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request)       { notImplemented(w, r) }
func (s *Server) adminGroups(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
func (s *Server) adminConnections(w http.ResponseWriter, r *http.Request) { notImplemented(w, r) }
func (s *Server) adminGrants(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
