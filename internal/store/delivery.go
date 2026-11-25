package store

import (
	"context"
	"database/sql"
	"strings"
)

type Delivery struct {
	ID             uint64 `json:"delivery_id"`
	DeviceID       uint64 `json:"-"`
	Epoch          uint64 `json:"-"`
	ConversationID uint64 `json:"conversation_id"`
	Seq            uint64 `json:"seq"`
	Kind           string `json:"-"`
}

func eventIDs(ids []uint64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// MaterializeDelivery commits the routing decisions and outbox acknowledgement together.
// Concurrent gateways lock only pending outbox rows and never wait on a socket.
func (s *Store) MaterializeDelivery(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM message_outbox WHERE live_delivery = TRUE AND dispatched_at IS NULL
		ORDER BY id LIMIT 64 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := make([]uint64, 0, 64)
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	in, args := eventIDs(ids)
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_attempts
		(outbox_id, tenant_id, app_id, node_id, device_id, epoch, membership_id)
		SELECT o.id, m.tenant_id, m.app_id, n.id, d.id, c.epoch, active.id
		FROM message_outbox AS o
		JOIN messages AS m ON m.conversation_id = o.conversation_id AND m.seq = o.seq
		JOIN memberships AS active ON active.conversation_id = o.conversation_id AND active.active_user_id IS NOT NULL
		JOIN devices AS d ON d.tenant_id = m.tenant_id AND d.app_id = m.app_id AND d.user_id = active.active_user_id
		JOIN device_connections AS c ON c.device_id = d.id AND c.epoch = d.connection_epoch
		JOIN gateway_nodes AS n ON n.id = c.node_id
		WHERE o.id IN (`+in+`) AND d.id <> COALESCE(o.origin_device_id, m.sender_device_id)
		AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)
		AND c.disconnected_at IS NULL AND c.credential_version = d.credential_version
		AND n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6)
		AND (o.kind <> 'read' OR o.recipient_user_id = d.user_id)
		AND (o.kind <> 'message' OR (m.expired_at IS NULL AND m.retracted_at IS NULL))
		AND EXISTS (SELECT 1 FROM memberships AS history
			WHERE history.tenant_id = m.tenant_id AND history.app_id = m.app_id AND history.conversation_id = m.conversation_id
			AND history.user_id = d.user_id AND history.from_seq <= m.seq AND (history.to_seq IS NULL OR history.to_seq >= m.seq))`, args...)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_outbox SET dispatched_at = UTC_TIMESTAMP(6) WHERE id IN (`+in+`)`, args...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) OfferDeliveries(ctx context.Context, node Node) ([]Delivery, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT a.id, a.device_id, a.epoch, o.conversation_id, o.seq, o.kind
		FROM delivery_attempts AS a JOIN message_outbox AS o ON o.id = a.outbox_id
		JOIN messages AS m ON m.conversation_id = o.conversation_id AND m.seq = o.seq
		JOIN devices AS d ON d.id = a.device_id AND d.connection_epoch = a.epoch
		JOIN device_connections AS c ON c.device_id = d.id AND c.epoch = a.epoch AND c.node_id = a.node_id
		JOIN gateway_nodes AS n ON n.id = a.node_id
		JOIN memberships AS active ON active.id = a.membership_id AND active.active_user_id = d.user_id
		WHERE a.node_id = ? AND a.state = 'queued' AND a.created_at > TIMESTAMPADD(SECOND, -30, UTC_TIMESTAMP(6))
		AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)
		AND c.disconnected_at IS NULL AND c.credential_version = d.credential_version
		AND n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6)
		AND (o.kind <> 'message' OR (m.expired_at IS NULL AND m.retracted_at IS NULL))
		ORDER BY a.id LIMIT 256 FOR UPDATE OF a SKIP LOCKED`, node.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []Delivery
	ids := make([]uint64, 0, 256)
	for rows.Next() {
		var item Delivery
		if err := rows.Scan(&item.ID, &item.DeviceID, &item.Epoch, &item.ConversationID, &item.Seq, &item.Kind); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	in, args := eventIDs(ids)
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET state = 'offered' WHERE id IN (`+in+`)`, args...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deliveries, nil
}

func (s *Store) MarkDeliverySent(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE delivery_attempts SET state = IF(state = 'offered', 'sent', state),
		sent_at = COALESCE(sent_at, UTC_TIMESTAMP(6)) WHERE id = ? AND state IN ('offered', 'acked')`, id)
	return err
}

func (s *Store) AcknowledgeDelivery(ctx context.Context, connection Connection, id uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE delivery_attempts SET state = 'acked',
		sent_at = COALESCE(sent_at, UTC_TIMESTAMP(6)), finished_at = GREATEST(sent_at, UTC_TIMESTAMP(6))
		WHERE id = ? AND node_id = ? AND device_id = ? AND epoch = ? AND state IN ('offered', 'sent')`,
		id, connection.Node.ID, connection.DeviceID, connection.Epoch)
	return err
}

func (s *Store) DegradeDeliveries(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	in, args := eventIDs(ids)
	_, err := s.db.ExecContext(ctx, `UPDATE delivery_attempts SET state = 'resync', finished_at = UTC_TIMESTAMP(6)
		WHERE id IN (`+in+`) AND state IN ('queued', 'offered', 'sent')`, args...)
	return err
}

func (s *Store) ExpireDeliveries(ctx context.Context) error {
	for _, stage := range []string{"queued", "offered", "sent"} {
		if err := s.expireDeliveryStage(ctx, stage); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) expireDeliveryStage(ctx context.Context, stage string) error {
	// Claim existing expired rows without locking index gaps used by live acknowledgements.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM delivery_attempts
		WHERE state = ? AND created_at <= TIMESTAMPADD(SECOND, -30, UTC_TIMESTAMP(6))
		ORDER BY created_at, id LIMIT 2000 FOR UPDATE SKIP LOCKED`, stage)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	in, args := eventIDs(ids)
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET state = 'resync', finished_at = UTC_TIMESTAMP(6)
		WHERE id IN (`+in+`)`, args...); err != nil {
		return err
	}
	return tx.Commit()
}
