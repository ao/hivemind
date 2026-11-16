package server

import (
	"net/http"
	"time"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// RegistryLogin is the body of PUT /v1/registries/{host}.
type RegistryLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegistryView is a stored registry credential, as the API shows it. It has
// no password field: no route returns a password.
type RegistryView struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryList is the reply to GET /v1/registries.
type RegistryList struct {
	Registries []RegistryView `json:"registries"`
}

func registryView(r store.Registry) RegistryView {
	return RegistryView{Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// pathRegistry returns the registry host in the path, in canonical form.
func pathRegistry(w http.ResponseWriter, r *http.Request) (string, bool) {
	h, err := spec.ParseRegistryHost(r.PathValue("host"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return h, true
}

// putRegistry serves PUT /v1/registries/{host}: it stores the credential
// that pulls from host send, or replaces the one stored. Running containers
// are not touched; the next pull uses it.
func (s *Server) putRegistry(w http.ResponseWriter, r *http.Request) {
	host, ok := pathRegistry(w, r)
	if !ok {
		return
	}
	var req RegistryLogin
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	reg, err := s.cfg.Store.PutRegistry(r.Context(), host, req.Username, req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Trigger()
	writeJSON(w, http.StatusOK, registryView(reg))
}

// listRegistries serves GET /v1/registries: each stored credential's host,
// username and times, without the password.
func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	regs, err := s.cfg.Store.ListRegistries(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := RegistryList{Registries: make([]RegistryView, 0, len(regs))}
	for _, reg := range regs {
		out.Registries = append(out.Registries, registryView(reg))
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteRegistry serves DELETE /v1/registries/{host}. Running containers
// keep running; the next pull from host sends no credential.
func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	host, ok := pathRegistry(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Store.DeleteRegistry(r.Context(), host); err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Trigger()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
