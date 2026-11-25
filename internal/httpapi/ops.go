package httpapi

import (
	"net/http"
	"strconv"

	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authAdmin(w, r)
	if !ok {
		return
	}
	window := 15
	if raw := r.URL.Query().Get("window_minutes"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1440 {
			problem(w, http.StatusBadRequest, "invalid_window")
			return
		}
		window = value
	}
	result, err := s.store.Diagnostics(r.Context(), scope, window)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, result)
}

func (s *Server) messageDelivery(w http.ResponseWriter, r *http.Request, scope store.Scope, id uint64) {
	seq, ok := parseID(r.PathValue("seq"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_sequence")
		return
	}
	result, err := s.store.MessageDelivery(r.Context(), scope, id, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, result)
}
