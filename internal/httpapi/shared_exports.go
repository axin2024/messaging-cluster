package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) previewSharedExport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	if !valid {
		problem(w, 422, "invalid_export_reference")
		return
	}
	preview, err := s.store.PreviewSharedExport(r.Context(), actor, link)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, preview)
}

func (s *Server) createSharedExport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	key := r.Header.Get("Idempotency-Key")
	var input store.SharedExportInput
	if !read(w, r, &input) {
		return
	}
	if !valid || !credential.ValidKey(key) {
		problem(w, 422, "invalid_export")
		return
	}
	job, err := s.store.CreateSharedExport(r.Context(), actor, link, key, input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v1/admin/partner-links/%d/exports/%d", link, job.ID))
	status := http.StatusOK
	if job.State == "queued" {
		status = http.StatusAccepted
	}
	write(w, status, job)
}

func (s *Server) exportRoute(handler func(http.ResponseWriter, *http.Request, store.Scope, uint64, uint64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.authSharedAdmin(w, r)
		if !ok {
			return
		}
		link, linkValid := parseID(r.PathValue("link"))
		id, idValid := parseID(r.PathValue("export"))
		if !linkValid || !idValid {
			problem(w, 422, "invalid_export_reference")
			return
		}
		handler(w, r, actor, link, id)
	}
}

func (s *Server) sharedExportStatus(w http.ResponseWriter, r *http.Request, actor store.Scope, link, id uint64) {
	job, err := s.store.SharedExportStatus(r.Context(), actor, link, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, job)
}

func (s *Server) retrySharedExport(w http.ResponseWriter, r *http.Request, actor store.Scope, link, id uint64) {
	if err := s.store.RetrySharedExport(r.Context(), actor, link, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) sharedExportPass(w http.ResponseWriter, r *http.Request, actor store.Scope, link, id uint64) {
	token, digest, err := credential.IssueToken()
	if err == nil {
		err = s.store.CreateSharedExportPass(r.Context(), actor, link, id, digest)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, map[string]any{"token": token, "expires_in": int64(store.SharedExportPassTTL / time.Second)})
}

func (s *Server) sharedExportContent(w http.ResponseWriter, r *http.Request, actor store.Scope, link, id uint64) {
	values := r.Header.Values("X-Export-Token")
	if len(values) != 1 {
		problem(w, 400, "export_token_required")
		return
	}
	token, ok := credential.TokenDigest(values[0])
	if !ok {
		problem(w, 400, "export_token_required")
		return
	}
	archive, sha, err := s.store.RedeemSharedExportPass(r.Context(), actor, link, id, token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="project-archive-%d.json"`, id))
	w.Header().Set("X-Archive-SHA256", sha)
	w.Header().Set("Content-Security-Policy", "sandbox")
	_, _ = w.Write(archive)
}
