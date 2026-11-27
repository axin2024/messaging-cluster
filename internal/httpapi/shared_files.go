package httpapi

import (
	"net/http"
	"time"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) sharedFileGrant(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, channelValid := parseID(r.PathValue("channel"))
	seq, seqValid := parseID(r.PathValue("seq"))
	file, fileValid := parseID(r.PathValue("attachment"))
	if !channelValid || !seqValid || !fileValid {
		problem(w, 422, "invalid_attachment_reference")
		return
	}
	if s.blobs == nil {
		problem(w, 503, "attachment_store_unavailable")
		return
	}
	token, digest, err := credential.IssueToken()
	if err == nil {
		err = s.store.CreateSharedFileGrant(r.Context(), session, channel, seq, file, digest)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, map[string]any{"token": token, "expires_in": int64(store.AttachmentGrantTTL / time.Second)})
}

func (s *Server) sharedFileContent(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	headers := r.Header.Values("X-Attachment-Token")
	if len(headers) != 1 {
		problem(w, 400, "attachment_token_required")
		return
	}
	digest, valid := credential.TokenDigest(headers[0])
	if !valid {
		problem(w, 400, "attachment_token_required")
		return
	}
	if s.blobs == nil {
		problem(w, 503, "attachment_store_unavailable")
		return
	}
	object, err := s.store.RedeemSharedFileGrant(r.Context(), session, digest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.serveAttachment(w, r, object)
}
