package store

import (
	"context"
	"database/sql"
	"errors"
)

type ReadState struct {
	Seq    uint64 `json:"read_seq"`
	Unread uint64 `json:"unread"`
}

// AcknowledgeSync is an explicit checkpoint after the client stores the returned
// page and any expiration gaps. Fetching a page alone never moves the checkpoint.
func (s *Store) AcknowledgeSync(ctx context.Context, session Session, id, seq uint64) (uint64, error) {
	return s.setPosition(ctx, session, id, seq, false)
}

func (s *Store) MarkRead(ctx context.Context, session Session, id, seq uint64) error {
	_, err := s.setPosition(ctx, session, id, seq, true)
	return err
}

func (s *Store) setPosition(ctx context.Context, session Session, id, seq uint64, shared bool) (uint64, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := lockConversation(ctx, tx, session.Scope, id); err != nil {
		return 0, err
	}
	var high, sync, read uint64
	err = tx.QueryRowContext(ctx, `SELECT i.high_seq, COALESCE(sc.seq, 0), COALESCE(r.seq, 0)
		FROM inbox AS i
		LEFT JOIN sync_cursors AS sc ON sc.conversation_id = i.conversation_id AND sc.device_id = ?
		LEFT JOIN read_state AS r ON r.tenant_id = i.tenant_id AND r.app_id = i.app_id AND r.user_id = i.user_id AND r.conversation_id = i.conversation_id
		WHERE i.tenant_id = ? AND i.app_id = ? AND i.user_id = ? AND i.conversation_id = ?`, session.DeviceID, session.TenantID,
		session.AppID, session.UserID, id).Scan(&high, &sync, &read)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	previous := sync
	if shared {
		previous = read
	}
	if seq <= previous {
		return previous, tx.Commit()
	}
	if seq > high {
		return 0, ErrForbidden
	}
	if err := checkVisibleSequence(ctx, tx, session, id, seq); err != nil {
		return 0, err
	}
	if shared {
		_, err = tx.ExecContext(ctx, `INSERT INTO read_state (tenant_id, app_id, user_id, conversation_id, seq)
			VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE seq = ?`, session.TenantID, session.AppID, session.UserID, id, seq, seq)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO message_outbox (conversation_id, seq, kind, recipient_user_id, origin_device_id)
				VALUES (?, ?, 'read', ?, ?)`, id, seq, session.UserID, session.DeviceID)
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_cursors (tenant_id, app_id, device_id, conversation_id, seq)
			VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE seq = ?`, session.TenantID, session.AppID, session.DeviceID, id, seq, seq)
	}
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func (s *Store) ReadState(ctx context.Context, session Session, id uint64) (ReadState, error) {
	query := `SELECT COALESCE(r.seq, 0), (SELECT COUNT(*) FROM messages AS m
			WHERE m.conversation_id = i.conversation_id AND m.seq >= c.retained_from_seq
			AND m.retracted_at IS NULL
			AND m.seq > COALESCE(r.seq, 0) AND m.seq <= i.high_seq AND m.sender_id <> i.user_id
			AND ` + messageVisibleToUser + `)
		FROM inbox AS i JOIN conversations AS c ON c.id = i.conversation_id
		LEFT JOIN read_state AS r ON r.tenant_id = i.tenant_id AND r.app_id = i.app_id AND r.user_id = i.user_id AND r.conversation_id = i.conversation_id
		WHERE i.tenant_id = ? AND i.app_id = ? AND i.user_id = ? AND i.conversation_id = ?`
	var state ReadState
	err := s.db.QueryRowContext(ctx, query, session.UserID, session.TenantID, session.AppID, session.UserID, id).Scan(&state.Seq, &state.Unread)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return state, err
}
