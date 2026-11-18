package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/ao/hivemind/internal/carotate"
)

// CARotationRequest is the body of POST /v1/ca/rotation/{step}. It may be
// empty. Force makes renew go ahead while live nodes do not trust the new
// CA, and finish cut off the nodes that have not renewed.
type CARotationRequest struct {
	Force bool `json:"force"`
}

// CARefusal is the reply, with 409, to a step the rotation's state or its
// nodes do not allow: the error, and the nodes it waits for.
type CARefusal struct {
	Error   string             `json:"error"`
	Waiting []carotate.Waiting `json:"waiting,omitempty"`
}

// getCA returns the cluster's CAs and the rotation in progress.
func (s *Server) getCA(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CA == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no cluster CA; run hivemind init")
		return
	}
	st, err := s.cfg.CA.Status(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// postCARotation runs one step of a rotation of the cluster CA.
func (s *Server) postCARotation(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CA == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no cluster CA; run hivemind init")
		return
	}
	var req CARotationRequest
	if r.ContentLength != 0 {
		if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
			writeDecodeError(w, err)
			return
		}
	}
	step := r.PathValue("step")
	var run func(context.Context) (carotate.Status, error)
	switch step {
	case "start":
		run = s.cfg.CA.Start
	case "renew":
		run = func(ctx context.Context) (carotate.Status, error) { return s.cfg.CA.Renew(ctx, req.Force) }
	case "finish":
		run = func(ctx context.Context) (carotate.Status, error) { return s.cfg.CA.Finish(ctx, req.Force) }
	case "abort":
		run = s.cfg.CA.Abort
	default:
		writeError(w, http.StatusNotFound, "no such step: the steps are start, renew, finish and abort")
		return
	}
	if req.Force && step != "renew" && step != "finish" {
		writeError(w, http.StatusBadRequest, "force applies to renew and finish only")
		return
	}
	// A step must not stop half way because the client went away; each
	// is short, and finishes on its own.
	st, err := run(context.WithoutCancel(r.Context()))
	if err != nil {
		var refused *carotate.RefusedError
		switch {
		case errors.As(err, &refused):
			writeJSON(w, http.StatusConflict, CARefusal{Error: err.Error(), Waiting: refused.Waiting})
		case carotate.IsRefusal(err):
			writeJSON(w, http.StatusConflict, CARefusal{Error: err.Error()})
		default:
			s.log.Error("CA rotation step failed", "step", step, "err", err)
			s.fail(w, r, err)
		}
		return
	}
	s.log.Info("CA rotation step", "step", step, "force", req.Force)
	writeJSON(w, http.StatusOK, st)
}
