package store

import (
	"context"
	"database/sql"
	"time"
)

type DeliveryCounts struct {
	Queued  uint64 `json:"queued"`
	Offered uint64 `json:"offered"`
	Sent    uint64 `json:"sent"`
	Acked   uint64 `json:"acked"`
	Resync  uint64 `json:"resync"`
}

type NodeHealth struct {
	Node
	Healthy             bool      `json:"healthy"`
	LeaseUntil          time.Time `json:"lease_until"`
	Active              uint64    `json:"active_connections"`
	Reconnects          uint64    `json:"reconnects"`
	FailedPings         uint64    `json:"heartbeat_timeouts"`
	PendingDeliveries   uint64    `json:"pending_deliveries"`
	OldestPendingMillis uint64    `json:"oldest_pending_ms"`
}

type Diagnostics struct {
	WindowMinutes      int            `json:"window_minutes"`
	MessagesPerSecond  int            `json:"messages_per_second"`
	CommittedMessages  uint64         `json:"committed_messages"`
	PendingOutbox      uint64         `json:"pending_outbox"`
	OldestOutboxMillis uint64         `json:"oldest_outbox_ms"`
	Delivery           DeliveryCounts `json:"delivery"`
	Nodes              []NodeHealth   `json:"nodes"`
}

func (s *Store) Diagnostics(ctx context.Context, scope Scope, window int) (Diagnostics, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Diagnostics{}, err
	}
	defer tx.Rollback()
	result := Diagnostics{WindowMinutes: window, Nodes: []NodeHealth{}}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT messages_per_second FROM tenant_message_limits WHERE tenant_id = ?), ?),
		(SELECT COUNT(*) FROM messages WHERE tenant_id = ? AND app_id = ? AND created_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6)))`,
		scope.TenantID, DefaultMessagesPerSecond, scope.TenantID, scope.AppID, -window).Scan(&result.MessagesPerSecond, &result.CommittedMessages)
	if err != nil {
		return Diagnostics{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(TIMESTAMPDIFF(MICROSECOND, MIN(o.created_at), UTC_TIMESTAMP(6)) DIV 1000, 0)
		FROM message_outbox AS o JOIN messages AS m ON m.conversation_id = o.conversation_id AND m.seq = o.seq
		WHERE o.live_delivery = TRUE AND o.dispatched_at IS NULL AND m.tenant_id = ? AND m.app_id = ?`, scope.TenantID, scope.AppID).
		Scan(&result.PendingOutbox, &result.OldestOutboxMillis)
	if err != nil {
		return Diagnostics{}, err
	}
	const counts = `COUNT(IF(a.state = 'queued', 1, NULL)), COUNT(IF(a.state = 'offered', 1, NULL)),
		COUNT(IF(a.state = 'sent', 1, NULL)), COUNT(IF(a.state = 'acked', 1, NULL)), COUNT(IF(a.state = 'resync', 1, NULL))`
	err = scanDeliveryCounts(tx.QueryRowContext(ctx, `SELECT `+counts+` FROM delivery_attempts AS a
		WHERE a.tenant_id = ? AND a.app_id = ? AND a.created_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6))`,
		scope.TenantID, scope.AppID, -window), &result.Delivery)
	if err != nil {
		return Diagnostics{}, err
	}
	rows, err := tx.QueryContext(ctx, `WITH backlog AS (
		SELECT node_id, COUNT(*) AS pending,
		TIMESTAMPDIFF(MICROSECOND, MIN(created_at), UTC_TIMESTAMP(6)) DIV 1000 AS oldest_ms
		FROM delivery_attempts WHERE tenant_id = ? AND app_id = ? AND state IN ('queued', 'offered', 'sent')
		GROUP BY node_id
	)
	SELECT n.id, n.node_name, n.lease_until,
		(n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6)) AS healthy,
		COUNT(IF(c.disconnected_at IS NULL AND n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6)
			AND d.connection_epoch = c.epoch AND d.credential_version = c.credential_version
			AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6), 1, NULL)),
		COUNT(IF(c.epoch > 1 AND c.connected_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6)), 1, NULL)),
		COUNT(IF(c.close_reason = 'heartbeat_timeout' AND c.disconnected_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6)), 1, NULL)),
		COALESCE(b.pending, 0), COALESCE(b.oldest_ms, 0)
		FROM device_connections AS c JOIN devices AS d ON d.id = c.device_id JOIN gateway_nodes AS n ON n.id = c.node_id
		LEFT JOIN backlog AS b ON b.node_id = n.id
		WHERE c.tenant_id = ? AND c.app_id = ? AND (c.connected_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6))
		OR c.disconnected_at >= TIMESTAMPADD(MINUTE, ?, UTC_TIMESTAMP(6)) OR c.disconnected_at IS NULL OR b.pending IS NOT NULL)
		GROUP BY n.id, n.node_name, n.lease_until, n.stopped_at, b.pending, b.oldest_ms ORDER BY n.id`,
		scope.TenantID, scope.AppID, -window, -window, scope.TenantID, scope.AppID, -window, -window)
	if err != nil {
		return Diagnostics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var node NodeHealth
		if err := rows.Scan(&node.ID, &node.Name, &node.LeaseUntil, &node.Healthy, &node.Active, &node.Reconnects, &node.FailedPings,
			&node.PendingDeliveries, &node.OldestPendingMillis); err != nil {
			return Diagnostics{}, err
		}
		result.Nodes = append(result.Nodes, node)
	}
	if err := rows.Err(); err != nil {
		return Diagnostics{}, err
	}
	return result, tx.Commit()
}

type MessageDelivery struct {
	ConversationID uint64         `json:"conversation_id"`
	Seq            uint64         `json:"seq"`
	LiveDelivery   bool           `json:"live_delivery"`
	CommittedAt    time.Time      `json:"committed_at"`
	DispatchedAt   *time.Time     `json:"dispatched_at,omitempty"`
	FirstSentAt    *time.Time     `json:"first_sent_at,omitempty"`
	FirstAckedAt   *time.Time     `json:"first_acked_at,omitempty"`
	Delivery       DeliveryCounts `json:"delivery"`
}

func (s *Store) MessageDelivery(ctx context.Context, scope Scope, id, seq uint64) (MessageDelivery, error) {
	result := MessageDelivery{ConversationID: id, Seq: seq}
	err := s.db.QueryRowContext(ctx, `SELECT o.live_delivery, m.created_at, o.dispatched_at, MIN(a.sent_at), MIN(IF(a.state = 'acked', a.finished_at, NULL)),
		COUNT(IF(a.state = 'queued', 1, NULL)), COUNT(IF(a.state = 'offered', 1, NULL)), COUNT(IF(a.state = 'sent', 1, NULL)),
		COUNT(IF(a.state = 'acked', 1, NULL)), COUNT(IF(a.state = 'resync', 1, NULL))
		FROM messages AS m JOIN message_outbox AS o ON o.conversation_id = m.conversation_id AND o.seq = m.seq AND o.kind = 'message'
		LEFT JOIN delivery_attempts AS a ON a.outbox_id = o.id
		WHERE m.tenant_id = ? AND m.app_id = ? AND m.conversation_id = ? AND m.seq = ?
		GROUP BY o.live_delivery, m.created_at, o.dispatched_at`, scope.TenantID, scope.AppID, id, seq).
		Scan(&result.LiveDelivery, &result.CommittedAt, &result.DispatchedAt, &result.FirstSentAt, &result.FirstAckedAt, &result.Delivery.Queued, &result.Delivery.Offered,
			&result.Delivery.Sent, &result.Delivery.Acked, &result.Delivery.Resync)
	if err == sql.ErrNoRows {
		return MessageDelivery{}, ErrNotFound
	}
	return result, err
}

func scanDeliveryCounts(row interface{ Scan(...any) error }, counts *DeliveryCounts) error {
	return row.Scan(&counts.Queued, &counts.Offered, &counts.Sent, &counts.Acked, &counts.Resync)
}
