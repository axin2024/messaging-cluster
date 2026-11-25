package store

import (
	"context"
	"database/sql"
)

const DefaultMessagesPerSecond = 250

func consumeSendQuota(ctx context.Context, tx *sql.Tx, tenant uint64) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO tenant_send_buckets (tenant_id, second_epoch, accepted)
		VALUES (?, UNIX_TIMESTAMP(), 1) ON DUPLICATE KEY UPDATE accepted = IF(accepted < COALESCE(
		(SELECT messages_per_second FROM tenant_message_limits WHERE tenant_id = ?), ?), accepted + 1, accepted)`,
		tenant, tenant, DefaultMessagesPerSecond)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrRateLimited
	}
	return nil
}

func (s *Store) SetTenantSendLimit(ctx context.Context, tenantKey string, limit int) error {
	var tenant uint64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE tenant_key = ?`, tenantKey).Scan(&tenant)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tenant_message_limits (tenant_id, messages_per_second)
		VALUES (?, ?) ON DUPLICATE KEY UPDATE messages_per_second = ?`, tenant, limit, limit)
	return err
}

func (s *Store) PruneSendBuckets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tenant_send_buckets WHERE second_epoch < UNIX_TIMESTAMP() - 120 LIMIT 2000`)
	return err
}
