package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type LegacyPublication struct {
	ClientID string `json:"client_id"`
	GroupID  uint64 `json:"group_id"`
	Seq      uint64 `json:"source_seq"`
}

func (s *Store) PreviewLegacyPublication(ctx context.Context, session Session, channel, group, seq uint64) (string, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return "", err
	}
	body, err := legacyPublicationText(ctx, tx, session, a, group, seq)
	if err != nil {
		return "", err
	}
	return body, tx.Commit()
}

func (s *Store) PublishLegacyMessage(ctx context.Context, session Session, channel uint64, input LegacyPublication) (uint64, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, true)
	if err != nil {
		return 0, err
	}
	body, err := legacyPublicationText(ctx, tx, session, a, input.GroupID, input.Seq)
	if err != nil {
		return 0, err
	}
	seq, err := appendSharedMessage(ctx, tx, session, a, SharedMessageInput{ClientID: input.ClientID, Body: body}, &input)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func legacyPublicationText(ctx context.Context, tx *sql.Tx, session Session, a sharedAccess, sourceGroup, sourceSeq uint64) (string, error) {
	if !a.host || a.Audience != "shared" {
		return "", ErrForbidden
	}
	group, actor, err := lockGroup(ctx, tx, session, sourceGroup)
	if err != nil {
		return "", err
	}
	if actor.Role != "owner" {
		return "", ErrForbidden
	}
	if err = checkVisibleSequence(ctx, tx, session, group.ID, sourceSeq); err != nil {
		return "", err
	}
	var body string
	err = tx.QueryRowContext(ctx, `SELECT body FROM messages WHERE conversation_id=? AND seq=? AND expired_at IS NULL AND retracted_at IS NULL`,
		group.ID, sourceSeq).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return "", ErrNoShareableText
	}
	return body, nil
}

func recordSharedOrigin(ctx context.Context, tx *sql.Tx, session Session, access sharedAccess, seq uint64, origin LegacyPublication) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO shared_message_origins (channel_id,seq,source_conversation,source_seq,actor_tenant,actor_app,actor_user)
		VALUES (?,?,?,?,?,?,?)`, access.ID, seq, origin.GroupID, origin.Seq, session.TenantID, session.AppID, session.UserID)
	if err != nil {
		return err
	}
	return recordSharedAudit(ctx, tx, access.LinkID, session.Scope, "legacy_message_published", access.epoch,
		map[string]any{"request_key": origin.ClientID, "source_conversation_id": origin.GroupID, "source_seq": origin.Seq,
			"channel_id": access.ID, "seq": seq, "actor_user_id": session.UserID})
}
