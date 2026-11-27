package store

import (
	"context"
	"database/sql"
	"time"
)

type MentionCandidate struct {
	UserID      uint64 `json:"user_id"`
	DisplayName string `json:"display_name"`
	TenantID    uint64 `json:"tenant_id"`
	Role        string `json:"duty_role"`
}

type SharedMention struct {
	Seq       uint64    `json:"seq"`
	Root      *uint64   `json:"root_seq,omitempty"`
	Sender    uint64    `json:"sender_user"`
	CreatedAt time.Time `json:"created_at"`
}

const mentionAudience = ` FROM shared_channels c JOIN partner_links l ON c.link_id=l.id
	JOIN shared_spaces s ON s.id=l.space_id JOIN shared_grants g ON g.link_id=l.id
	WHERE c.id=? AND g.revoked_at IS NULL AND g.expires_at>UTC_TIMESTAMP(6)
	AND ((g.tenant_id=s.host_tenant AND g.app_id=s.host_app) OR (c.audience='shared' AND g.tenant_id=l.peer_tenant
	AND g.app_id=l.peer_app AND 'project:read' MEMBER OF(l.accepted_scope)))
	AND (?=0 OR EXISTS (SELECT 1 FROM shared_messages m WHERE m.channel_id=c.id AND m.seq=?
	AND m.root_seq IS NULL AND (m.retained_until IS NULL OR m.retained_until>UTC_TIMESTAMP(6))
	AND m.hidden_at IS NULL AND m.project_seq>=` + sharedHistoryFloor + `))`

func addSharedMentions(ctx context.Context, tx *sql.Tx, a sharedAccess, seq uint64, root *uint64, ids []uint64) error {
	var rootSeq uint64
	if root != nil {
		rootSeq = *root
	}
	for _, user := range ids {
		result, err := tx.ExecContext(ctx, `INSERT INTO shared_message_mentions (channel_id,seq,recipient_tenant,recipient_app,recipient_user)
			SELECT c.id,?,g.tenant_id,g.app_id,g.user_id`+mentionAudience+` AND g.user_id=?`, seq, a.ID, rootSeq, rootSeq, user)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrMentionUnavailable
		}
	}
	return nil
}

func (s *Store) SharedMentionCandidates(ctx context.Context, session Session, channel, root, after uint64, side, role string) ([]MentionCandidate, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return nil, err
	}
	if root != 0 {
		if err = checkSharedReference(ctx, tx, a, root, true); err != nil {
			return nil, err
		}
	}
	operator := "="
	if side == "partner" {
		operator = "<>"
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.user_id,
		(SELECT u.display_name FROM users u WHERE u.id=g.user_id AND u.app_id=g.app_id AND u.tenant_id=g.tenant_id),g.tenant_id,g.duty_role`+
		mentionAudience+` AND g.tenant_id`+operator+`? AND g.user_id>? AND g.user_id<>? AND (?='' OR g.duty_role=?) ORDER BY g.user_id LIMIT 100`,
		channel, root, root, session.TenantID, after, session.UserID, role, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []MentionCandidate{}
	for rows.Next() {
		var item MentionCandidate
		if err = rows.Scan(&item.UserID, &item.DisplayName, &item.TenantID, &item.Role); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}

func (s *Store) SharedMentions(ctx context.Context, session Session, channel, after uint64) ([]SharedMention, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.seq,m.root_seq,m.sender_user,m.created_at FROM shared_message_mentions n
		JOIN shared_messages m ON m.channel_id=n.channel_id AND m.seq=n.seq
		WHERE n.channel_id=? AND n.recipient_tenant=? AND n.recipient_app=? AND n.recipient_user=? AND n.seq>? AND `+visibleSharedMessage("?")+
		` ORDER BY n.seq LIMIT 100`, channel, session.TenantID, session.AppID, session.UserID, after, a.floor, a.floor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SharedMention{}
	for rows.Next() {
		var item SharedMention
		if err = rows.Scan(&item.Seq, &item.Root, &item.Sender, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = recordSharedDevice(ctx, tx, session, a); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
