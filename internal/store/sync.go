package store

import (
	"context"
	"database/sql"
	"errors"
)

type InboxEntry struct {
	ConversationID uint64 `json:"conversation_id"`
	HighSeq        uint64 `json:"high_seq"`
	SyncSeq        uint64 `json:"sync_seq"`
	ReadSeq        uint64 `json:"read_seq"`
}

type SequenceRange struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

type SyncPage struct {
	Items             []Message       `json:"items"`
	NextAfter         uint64          `json:"next_after"`
	HasMore           bool            `json:"has_more"`
	Visible           []SequenceRange `json:"visible"`
	Expired           []SequenceRange `json:"expired"`
	EarliestAvailable uint64          `json:"earliest_available,omitempty"`
}

func upsertInbox(ctx context.Context, tx *sql.Tx, scope Scope, user, id, high uint64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbox (tenant_id, app_id, user_id, conversation_id, high_seq)
		VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE high_seq = GREATEST(high_seq, ?)`, scope.TenantID, scope.AppID, user, id, high, high)
	return err
}

func advanceInboxForMessage(ctx context.Context, tx *sql.Tx, scope Scope, id, seq uint64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbox (tenant_id, app_id, user_id, conversation_id, high_seq)
		SELECT tenant_id, app_id, active_user_id, conversation_id, ? FROM memberships
		WHERE tenant_id = ? AND app_id = ? AND conversation_id = ? AND active_user_id IS NOT NULL
		ORDER BY active_user_id ON DUPLICATE KEY UPDATE high_seq = ?`, seq, scope.TenantID, scope.AppID, id, seq)
	return err
}

func (s *Store) Inbox(ctx context.Context, session Session, after uint64) ([]InboxEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.conversation_id, i.high_seq, COALESCE(sc.seq, 0), COALESCE(r.seq, 0)
		FROM inbox AS i
		LEFT JOIN sync_cursors AS sc ON sc.conversation_id = i.conversation_id AND sc.device_id = ?
		LEFT JOIN read_state AS r ON r.tenant_id = i.tenant_id AND r.app_id = i.app_id AND r.user_id = i.user_id AND r.conversation_id = i.conversation_id
		WHERE i.tenant_id = ? AND i.app_id = ? AND i.user_id = ? AND i.conversation_id > ?
		ORDER BY i.conversation_id LIMIT 50`, session.DeviceID, session.TenantID, session.AppID, session.UserID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]InboxEntry, 0)
	for rows.Next() {
		var entry InboxEntry
		if err := rows.Scan(&entry.ConversationID, &entry.HighSeq, &entry.SyncSeq, &entry.ReadSeq); err != nil {
			return nil, err
		}
		items = append(items, entry)
	}
	return items, rows.Err()
}

func checkVisibleSequence(ctx context.Context, tx *sql.Tx, session Session, id, seq uint64) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM memberships WHERE `+membershipScope+`
		AND from_seq <= ? AND (to_seq IS NULL OR to_seq >= ?) LIMIT 1`, session.TenantID, session.AppID, id, session.UserID, seq, seq).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	return err
}

func visibleRanges(ctx context.Context, tx *sql.Tx, session Session, id, high uint64) ([]SequenceRange, error) {
	rows, err := tx.QueryContext(ctx, `SELECT from_seq, COALESCE(to_seq, ?) FROM memberships WHERE `+membershipScope+`
		AND from_seq <= ? ORDER BY from_seq`, high, session.TenantID, session.AppID, id, session.UserID, high)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranges := make([]SequenceRange, 0)
	for rows.Next() {
		var current SequenceRange
		if err := rows.Scan(&current.From, &current.To); err != nil {
			return nil, err
		}
		if current.To < current.From {
			continue
		}
		if len(ranges) > 0 && current.From-1 <= ranges[len(ranges)-1].To {
			last := &ranges[len(ranges)-1]
			last.To = max(last.To, current.To)
			continue
		}
		ranges = append(ranges, current)
	}
	return ranges, rows.Err()
}

// Sync has a single snapshot for the visible high-water mark, retained floor and
// page contents. A nil cursor resumes from the last explicit device acknowledgement.
func (s *Store) Sync(ctx context.Context, session Session, id uint64, after *uint64, limit int) (SyncPage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return SyncPage{}, err
	}
	defer tx.Rollback()
	var high, retainedFrom, checkpoint uint64
	err = tx.QueryRowContext(ctx, `SELECT i.high_seq, c.retained_from_seq, COALESCE(sc.seq, 0)
		FROM inbox AS i JOIN conversations AS c ON c.id = i.conversation_id
		LEFT JOIN sync_cursors AS sc ON sc.conversation_id = i.conversation_id AND sc.device_id = ?
		WHERE i.tenant_id = ? AND i.app_id = ? AND i.user_id = ? AND i.conversation_id = ?`, session.DeviceID,
		session.TenantID, session.AppID, session.UserID, id).Scan(&high, &retainedFrom, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncPage{}, ErrNotFound
	}
	if err != nil {
		return SyncPage{}, err
	}
	if after != nil {
		checkpoint = *after
	}
	ranges, err := visibleRanges(ctx, tx, session, id, high)
	if err != nil {
		return SyncPage{}, err
	}
	page := SyncPage{Items: []Message{}, Visible: ranges, Expired: []SequenceRange{}, NextAfter: checkpoint}
	for _, visible := range ranges {
		if page.EarliestAvailable == 0 && visible.To >= retainedFrom {
			page.EarliestAvailable = max(visible.From, retainedFrom)
		}
		missingTo := min(visible.To, retainedFrom-1)
		if missingTo > checkpoint && missingTo >= visible.From {
			page.Expired = append(page.Expired, SequenceRange{From: max(visible.From, checkpoint+1), To: missingTo})
			page.NextAfter = missingTo
		}
	}
	if checkpoint < high && page.EarliestAvailable > 0 {
		items, err := visibleMessages(ctx, tx, session, id, max(checkpoint+1, page.EarliestAvailable), limit+1)
		if err != nil {
			return SyncPage{}, err
		}
		page.HasMore = len(items) > limit
		if page.HasMore {
			items = items[:limit]
		}
		if len(items) > 0 {
			page.NextAfter = items[len(items)-1].Seq
		}
		page.Items = items
	}
	return page, tx.Commit()
}
