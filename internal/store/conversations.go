package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Conversation struct {
	ID            uint64    `json:"id"`
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	HistoryPolicy string    `json:"history_policy"`
	DirectLow     *uint64   `json:"direct_low,omitempty"`
	DirectHigh    *uint64   `json:"direct_high,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	lastSeq       uint64
	retainedFrom  uint64
}

const conversationColumns = "c.id, c.kind, c.title, c.history_policy, c.direct_low, c.direct_high, c.created_at, c.last_seq, c.retained_from_seq"

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var value Conversation
	err := row.Scan(&value.ID, &value.Kind, &value.Title, &value.HistoryPolicy, &value.DirectLow, &value.DirectHigh, &value.CreatedAt, &value.lastSeq, &value.retainedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return value, err
}

// Conversation writers lock the device before the conversation, then read roles
// and allocate sequence boundaries against the current committed rows.
func (s *Store) beginConversationWrite(ctx context.Context, session Session) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	var ignored uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id = ? AND app_id = ? AND id = ? FOR SHARE`,
		session.TenantID, session.AppID, session.UserID).Scan(&ignored)
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT id FROM devices
			WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ? AND credential_version = ?
			AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(6) FOR SHARE`,
			session.TenantID, session.AppID, session.UserID, session.DeviceID, session.CredentialVersion).Scan(&ignored)
	}
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrUnauthorized
		}
		return nil, err
	}
	return tx, nil
}

func lockConversation(ctx context.Context, tx *sql.Tx, scope Scope, id uint64) (Conversation, error) {
	return scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations AS c
		WHERE c.tenant_id = ? AND c.app_id = ? AND c.id = ? FOR UPDATE`, scope.TenantID, scope.AppID, id))
}

func scopedUser(ctx context.Context, tx *sql.Tx, scope Scope, id uint64) error {
	var userID uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id = ? AND app_id = ? AND id = ?`,
		scope.TenantID, scope.AppID, id).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) OpenDirect(ctx context.Context, session Session, peer uint64) (Conversation, error) {
	if peer == session.UserID {
		return Conversation{}, ErrForbidden
	}
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	if err := scopedUser(ctx, tx, session.Scope, peer); err != nil {
		return Conversation{}, err
	}
	low, high := min(peer, session.UserID), max(peer, session.UserID)
	result, err := tx.ExecContext(ctx, `INSERT INTO conversations (tenant_id, app_id, kind, history_policy, direct_low, direct_high)
		VALUES (?, ?, 'direct', 'all', ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
		session.TenantID, session.AppID, low, high)
	if err != nil {
		return Conversation{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Conversation{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, app_id, conversation_id, user_id, role, from_seq)
		VALUES (?, ?, ?, ?, 'member', 1), (?, ?, ?, ?, 'member', 1)
		ON DUPLICATE KEY UPDATE user_id = memberships.user_id`, session.TenantID, session.AppID, id, low, session.TenantID, session.AppID, id, high)
	if err != nil {
		return Conversation{}, err
	}
	for _, user := range []uint64{low, high} {
		if err := upsertInbox(ctx, tx, session.Scope, user, uint64(id), 0); err != nil {
			return Conversation{}, err
		}
	}
	conversation, err := lockConversation(ctx, tx, session.Scope, uint64(id))
	if err != nil {
		return Conversation{}, err
	}
	return conversation, tx.Commit()
}

func (s *Store) CreateGroup(ctx context.Context, session Session, title, historyPolicy string) (Conversation, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO conversations (tenant_id, app_id, kind, title, history_policy)
		VALUES (?, ?, 'group', ?, ?)`, session.TenantID, session.AppID, title, historyPolicy)
	if err != nil {
		return Conversation{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Conversation{}, err
	}
	if err := insertMember(ctx, tx, session.Scope, uint64(id), session.UserID, "owner", 1); err != nil {
		return Conversation{}, err
	}
	if err := upsertInbox(ctx, tx, session.Scope, session.UserID, uint64(id), 0); err != nil {
		return Conversation{}, err
	}
	conversation, err := lockConversation(ctx, tx, session.Scope, uint64(id))
	if err != nil {
		return Conversation{}, err
	}
	return conversation, tx.Commit()
}

func (s *Store) Conversations(ctx context.Context, session Session, after uint64) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+` FROM (
		SELECT DISTINCT conversation_id FROM memberships WHERE tenant_id = ? AND app_id = ? AND user_id = ?
			AND conversation_id > ? ORDER BY conversation_id LIMIT 50
		) AS visible JOIN conversations AS c ON c.id = visible.conversation_id ORDER BY c.id`,
		session.TenantID, session.AppID, session.UserID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Conversation, 0)
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
