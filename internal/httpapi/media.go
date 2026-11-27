package httpapi

import (
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/media"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) createAttachment(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	if s.blobs == nil {
		problem(w, http.StatusServiceUnavailable, "attachment_store_unavailable")
		return
	}
	var input struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		SHA256      string `json:"sha256"`
	}
	if !read(w, r, &input) {
		return
	}
	filename := strings.TrimSpace(input.Filename)
	contentType, params, typeErr := mime.ParseMediaType(input.ContentType)
	major, minor, hasSlash := strings.Cut(contentType, "/")
	digest, digestErr := hex.DecodeString(input.SHA256)
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) ||
		strings.IndexFunc(filename, unicode.IsControl) >= 0 || utf8.RuneCountInString(filename) > 180 ||
		!hasSlash || major == "" || minor == "" || strings.Contains(contentType, "*") || len(contentType) > 127 || len(params) != 0 || typeErr != nil ||
		input.Size < 1 || input.Size > media.MaxBytes || digestErr != nil || len(digest) != 32 {
		problem(w, http.StatusBadRequest, "invalid_attachment")
		return
	}
	attachment, err := s.store.CreateAttachment(r.Context(), session, filename, contentType, input.Size, [32]byte(digest))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, attachment)
}

func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSession(w, r)
	if !ok {
		return
	}
	if s.blobs == nil {
		problem(w, http.StatusServiceUnavailable, "attachment_store_unavailable")
		return
	}
	id, ok := parseID(r.PathValue("attachment"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_attachment")
		return
	}
	attachment, err := s.store.UploadAttachment(r.Context(), session, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if attachment.ReadyAt != nil {
		write(w, http.StatusOK, attachment)
		return
	}
	if r.ContentLength > media.MaxBytes {
		problem(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != attachment.Size {
		problem(w, http.StatusUnprocessableEntity, "attachment_size_mismatch")
		return
	}
	select {
	case s.uploadJobs <- struct{}{}:
		defer func() { <-s.uploadJobs }()
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusTooManyRequests, "upload_busy")
		return
	}
	body := http.MaxBytesReader(w, r.Body, media.MaxBytes)
	defer body.Close()
	key, err := s.blobs.Put(body, attachment.Size, attachment.Digest)
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			problem(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		case errors.Is(err, media.ErrLength):
			problem(w, http.StatusUnprocessableEntity, "attachment_size_mismatch")
		case errors.Is(err, media.ErrDigest):
			problem(w, http.StatusUnprocessableEntity, "attachment_digest_mismatch")
		default:
			s.fail(w, r, err)
		}
		return
	}
	attachment, stored, err := s.store.CompleteAttachment(r.Context(), session, id, key)
	if !stored {
		if discardErr := s.blobs.Discard(key); discardErr != nil {
			s.logger.Warn("could not remove unused attachment attempt", "attachment", id, "error", discardErr)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, attachment)
}

func (s *Server) attachmentGrant(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	seq, seqValid := parseID(r.PathValue("seq"))
	attachment, attachmentValid := parseID(r.PathValue("attachment"))
	if !seqValid || !attachmentValid {
		problem(w, http.StatusBadRequest, "invalid_attachment_reference")
		return
	}
	token, digest, err := credential.IssueToken()
	if err == nil {
		err = s.store.CreateAttachmentGrant(r.Context(), session, id, seq, attachment, digest)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
	}{Token: token, ExpiresIn: int64(store.AttachmentGrantTTL / time.Second)})
}

func (s *Server) attachmentContent(w http.ResponseWriter, r *http.Request) {
	headers := r.Header.Values("Authorization")
	var digest [32]byte
	var valid bool
	if len(headers) == 1 {
		scheme, token, ok := strings.Cut(headers[0], " ")
		if ok && strings.EqualFold(scheme, "Attachment") {
			digest, valid = credential.TokenDigest(token)
		}
	}
	if !valid {
		w.Header().Set("WWW-Authenticate", "Attachment")
		problem(w, http.StatusUnauthorized, "attachment_token_required")
		return
	}
	if s.blobs == nil {
		problem(w, http.StatusServiceUnavailable, "attachment_store_unavailable")
		return
	}
	attachment, err := s.store.RedeemAttachmentGrant(r.Context(), digest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.serveAttachment(w, r, attachment)
}

func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request, attachment store.Attachment) {
	content, err := s.blobs.Open(*attachment.ObjectKey, attachment.Size)
	if err != nil {
		s.logger.Error("attachment file unavailable", "attachment", attachment.ID, "error", err)
		problem(w, http.StatusServiceUnavailable, "attachment_store_unavailable")
		return
	}
	defer content.Close()
	w.Header().Set("Content-Type", attachment.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Filename}))
	w.Header().Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, attachment.Filename, *attachment.ReadyAt, content)
}

func (s *Server) retractMessage(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	seq, ok := parseID(r.PathValue("seq"))
	if !ok {
		problem(w, http.StatusBadRequest, "invalid_sequence")
		return
	}
	record, err := s.store.Retract(r.Context(), session, id, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, record)
}

func (s *Server) retractions(w http.ResponseWriter, r *http.Request, session store.Session, id uint64) {
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	items, err := s.store.Retractions(r.Context(), session, id, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result := page[store.Retraction]{Items: items, NextAfter: after}
	if len(items) > 0 {
		result.NextAfter = items[len(items)-1].ID
	}
	write(w, http.StatusOK, result)
}
