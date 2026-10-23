package httpx

import "net/http"

// Owner: C. Setiap handler wajib memanggil s.Authz.Can dengan aksi yang sesuai.

func (s *Server) dbSchema(w http.ResponseWriter, r *http.Request)          { notImplemented(w, r) } // schema.view
func (s *Server) dbTable(w http.ResponseWriter, r *http.Request)           { notImplemented(w, r) } // schema.view
func (s *Server) dbData(w http.ResponseWriter, r *http.Request)            { notImplemented(w, r) } // data.view
func (s *Server) migrationList(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) } // migration.view
func (s *Server) migrationDetail(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) } // migration.view
func (s *Server) migrationNewForm(w http.ResponseWriter, r *http.Request)  { notImplemented(w, r) } // migration.create
func (s *Server) migrationCreate(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) } // migration.create
func (s *Server) migrationEditForm(w http.ResponseWriter, r *http.Request) { notImplemented(w, r) } // migration.update
func (s *Server) migrationUpdate(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) } // migration.update
func (s *Server) migrationExecute(w http.ResponseWriter, r *http.Request)  { notImplemented(w, r) } // migration.execute
