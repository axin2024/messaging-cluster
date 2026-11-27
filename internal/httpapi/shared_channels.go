package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func (s *Server) createSharedChannel(w http.ResponseWriter, r *http.Request) {
	host, ok := s.authSharedAdmin(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	var input struct {
		Key         string `json:"key"`
		Title       string `json:"title"`
		Audience    string `json:"audience"`
		HistoryFrom string `json:"history_from"`
	}
	if !read(w, r, &input) {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if !valid || !credential.ValidKey(input.Key) || input.Title == "" || utf8.RuneCountInString(input.Title) > 160 ||
		(input.Audience != "host" && input.Audience != "shared") || (input.HistoryFrom != "join" && input.HistoryFrom != "beginning") {
		problem(w, 422, "invalid_channel")
		return
	}
	channel, err := s.store.CreateSharedChannel(r.Context(), host, link, store.SharedChannel{
		Key: input.Key, Title: input.Title, Audience: input.Audience, HistoryFrom: input.HistoryFrom,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, channel)
}

func (s *Server) sharedChannels(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	link, valid := parseID(r.PathValue("link"))
	var after uint64
	if value := r.URL.Query().Get("after"); value != "" {
		var err error
		after, err = strconv.ParseUint(value, 10, 64)
		valid = valid && err == nil
	}
	if !valid {
		problem(w, 422, "invalid_channel_page")
		return
	}
	channels, err := s.store.ListSharedChannels(r.Context(), session, link, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]any{"items": channels})
}

func (s *Server) sendSharedMessage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	var input store.SharedMessageInput
	if !read(w, r, &input) {
		return
	}
	if !valid || !credential.ValidMessage(input.ClientID, input.Body, input.Attachments) || len(input.Mentions) > 20 ||
		(input.Root != nil && *input.Root == 0) || (input.Quote != nil && *input.Quote == 0) ||
		(input.QuoteChannel != nil && (input.Quote == nil || *input.QuoteChannel == 0)) {
		problem(w, 422, "invalid_shared_message")
		return
	}
	slices.Sort(input.Mentions)
	for i, user := range input.Mentions {
		if user == 0 || user == session.UserID || (i > 0 && user == input.Mentions[i-1]) {
			problem(w, 422, "invalid_mention_recipients")
			return
		}
	}
	seq, err := s.store.SendSharedMessage(r.Context(), session, channel, input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, map[string]uint64{"seq": seq})
}

func (s *Server) publishLegacyMessage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	var input store.LegacyPublication
	if !read(w, r, &input) {
		return
	}
	if !valid || !credential.ValidKey(input.ClientID) || input.GroupID == 0 || input.Seq == 0 {
		problem(w, 422, "invalid_source_message")
		return
	}
	seq, err := s.store.PublishLegacyMessage(r.Context(), session, channel, input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 201, map[string]uint64{"seq": seq})
}

func (s *Server) previewLegacyPublication(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	group, groupValid := parseID(r.URL.Query().Get("group_id"))
	seq, seqValid := parseID(r.URL.Query().Get("source_seq"))
	if !valid || !groupValid || !seqValid {
		problem(w, 422, "invalid_source_message")
		return
	}
	body, err := s.store.PreviewLegacyPublication(r.Context(), session, channel, group, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, map[string]string{"body": body})
}

func (s *Server) sharedTimeline(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authSharedSession(w, r)
	if !ok {
		return
	}
	channel, valid := parseID(r.PathValue("channel"))
	var thread uint64
	if value := r.PathValue("root"); value != "" {
		var ok bool
		thread, ok = parseID(value)
		valid = valid && ok
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		valid = valid && err == nil && limit >= 1 && limit <= 100
	}
	if !valid {
		problem(w, 422, "invalid_shared_page")
		return
	}
	page, err := s.store.SharedTimeline(r.Context(), session, channel, thread, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	write(w, 200, page)
}
