package store

import (
	"context"
	"time"
)

const retentionBatch = 1000

type RetentionRun struct {
	ID              uint64    `json:"id"`
	ConversationID  uint64    `json:"conversation_id"`
	RequestedBefore time.Time `json:"requested_before"`
	SequenceRange
	AppliedAt time.Time `json:"applied_at"`
}

type RetentionResult struct {
	RetainedFrom uint64        `json:"retained_from"`
	HasMore      bool          `json:"has_more"`
	Run          *RetentionRun `json:"run,omitempty"`
}

func (s *Store) ExpireHistory(ctx context.Context, scope Scope, id uint64, before time.Time) (RetentionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RetentionResult{}, err
	}
	defer tx.Rollback()
	conversation, err := lockConversation(ctx, tx, scope, id)
	if err != nil {
		return RetentionResult{}, err
	}
	result := RetentionResult{RetainedFrom: conversation.retainedFrom}
	rows, err := tx.QueryContext(ctx, `SELECT seq, created_at < ? FROM messages
		WHERE conversation_id = ? AND seq >= ? ORDER BY seq LIMIT ?`, before, id, conversation.retainedFrom, retentionBatch+1)
	if err != nil {
		return RetentionResult{}, err
	}
	defer rows.Close()
	var through uint64
	for count := 0; rows.Next(); count++ {
		var seq uint64
		var expired bool
		if err := rows.Scan(&seq, &expired); err != nil {
			return RetentionResult{}, err
		}
		if !expired {
			break
		}
		if count == retentionBatch {
			result.HasMore = true
			break
		}
		through = seq
	}
	if err := rows.Close(); err != nil {
		return RetentionResult{}, err
	}
	if err := rows.Err(); err != nil {
		return RetentionResult{}, err
	}
	if through == 0 {
		return result, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = '', expired_at = UTC_TIMESTAMP(6)
		WHERE conversation_id = ? AND seq BETWEEN ? AND ?`, id, conversation.retainedFrom, through); err != nil {
		return RetentionResult{}, err
	}
	result.RetainedFrom = through + 1
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET retained_from_seq = ? WHERE id = ?`, result.RetainedFrom, id); err != nil {
		return RetentionResult{}, err
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO retention_runs (tenant_id, app_id, conversation_id, requested_before, from_seq, to_seq)
		VALUES (?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.AppID, id, before, conversation.retainedFrom, through)
	if err != nil {
		return RetentionResult{}, err
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RetentionResult{}, err
	}
	run, err := scanRetention(tx.QueryRowContext(ctx, `SELECT id, conversation_id, requested_before, from_seq, to_seq, applied_at
		FROM retention_runs WHERE id = ?`, runID))
	if err != nil {
		return RetentionResult{}, err
	}
	result.Run = &run
	return result, tx.Commit()
}

func (s *Store) RetentionRuns(ctx context.Context, scope Scope, id, after uint64) ([]RetentionRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id, requested_before, from_seq, to_seq, applied_at
		FROM retention_runs WHERE tenant_id = ? AND app_id = ? AND conversation_id = ? AND id > ? ORDER BY id LIMIT 50`,
		scope.TenantID, scope.AppID, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]RetentionRun, 0)
	for rows.Next() {
		run, err := scanRetention(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, run)
	}
	return items, rows.Err()
}

func scanRetention(row interface{ Scan(...any) error }) (RetentionRun, error) {
	var run RetentionRun
	err := row.Scan(&run.ID, &run.ConversationID, &run.RequestedBefore, &run.From, &run.To, &run.AppliedAt)
	return run, err
}
