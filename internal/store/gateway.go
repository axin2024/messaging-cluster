package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Node struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type Connection struct {
	DeviceID       uint64     `json:"device_id"`
	Epoch          uint64     `json:"connection_epoch"`
	Node           Node       `json:"node"`
	ConnectedAt    time.Time  `json:"connected_at"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	CloseReason    string     `json:"close_reason,omitempty"`
}

type Presence struct {
	DeviceID   uint64      `json:"device_id"`
	Online     bool        `json:"online"`
	Status     string      `json:"status"`
	LastSeenAt *time.Time  `json:"last_seen_at,omitempty"`
	Connection *Connection `json:"connection,omitempty"`
}

func (s *Store) OpenNode(ctx context.Context, name string, lease time.Duration) (Node, error) {
	const query = "INSERT INTO gateway_nodes (node_name, lease_until) VALUES (?, TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)))"
	result, err := s.db.ExecContext(ctx, query, name, lease.Microseconds())
	if err != nil {
		return Node{}, err
	}
	id, err := result.LastInsertId()
	return Node{ID: uint64(id), Name: name}, err
}

func (s *Store) RenewNode(ctx context.Context, node Node, lease time.Duration) error {
	const query = `
		UPDATE gateway_nodes SET last_heartbeat_at = UTC_TIMESTAMP(6), lease_until = TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6))
		WHERE id = ? AND stopped_at IS NULL AND lease_until > UTC_TIMESTAMP(6)`
	result, err := s.db.ExecContext(ctx, query, lease.Microseconds(), node.ID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err == nil && updated != 1 {
		return ErrLeaseLost
	}
	return err
}

func (s *Store) StopNode(ctx context.Context, node Node) error {
	_, err := s.db.ExecContext(ctx, "UPDATE gateway_nodes SET stopped_at = UTC_TIMESTAMP(6) WHERE id = ? AND stopped_at IS NULL", node.ID)
	return err
}

func (s *Store) Connect(ctx context.Context, node Node, session Session) (Connection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	const device = `
		SELECT connection_epoch FROM devices
		WHERE tenant_id = ? AND app_id = ? AND user_id = ? AND id = ?
			AND credential_version = ? AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(6)
		FOR UPDATE`
	var previous uint64
	err = tx.QueryRowContext(ctx, device, session.TenantID, session.AppID, session.UserID, session.DeviceID, session.CredentialVersion).
		Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrUnauthorized
	}
	if err != nil {
		return Connection{}, err
	}
	const checkNode = "SELECT EXISTS (SELECT 1 FROM gateway_nodes WHERE id = ? AND stopped_at IS NULL AND lease_until > UTC_TIMESTAMP(6))"
	var active bool
	if err := tx.QueryRowContext(ctx, checkNode, node.ID).Scan(&active); err != nil {
		return Connection{}, err
	}
	if !active {
		return Connection{}, ErrLeaseLost
	}
	current := Connection{DeviceID: session.DeviceID, Epoch: previous + 1, Node: node}
	if previous > 0 {
		const supersede = `
			UPDATE device_connections SET disconnected_at = UTC_TIMESTAMP(6), close_reason = 'superseded'
			WHERE tenant_id = ? AND app_id = ? AND device_id = ? AND epoch = ? AND disconnected_at IS NULL`
		if _, err := tx.ExecContext(ctx, supersede, session.TenantID, session.AppID, session.DeviceID, previous); err != nil {
			return Connection{}, err
		}
	}
	const advance = "UPDATE devices SET connection_epoch = ? WHERE tenant_id = ? AND app_id = ? AND id = ?"
	if _, err := tx.ExecContext(ctx, advance, current.Epoch, session.TenantID, session.AppID, session.DeviceID); err != nil {
		return Connection{}, err
	}
	const insert = `
		INSERT INTO device_connections (tenant_id, app_id, device_id, epoch, node_id, credential_version)
		VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insert, session.TenantID, session.AppID, session.DeviceID, current.Epoch, node.ID, session.CredentialVersion); err != nil {
		return Connection{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT connected_at FROM device_connections WHERE device_id = ? AND epoch = ?", current.DeviceID, current.Epoch).
		Scan(&current.ConnectedAt); err != nil {
		return Connection{}, err
	}
	return current, tx.Commit()
}

func (s *Store) Disconnect(ctx context.Context, connection Connection, reason string) error {
	const query = `
		UPDATE device_connections SET disconnected_at = UTC_TIMESTAMP(6), close_reason = ?
		WHERE device_id = ? AND epoch = ? AND node_id = ? AND disconnected_at IS NULL`
	_, err := s.db.ExecContext(ctx, query, reason, connection.DeviceID, connection.Epoch, connection.Node.ID)
	return err
}

func (s *Store) LiveConnections(ctx context.Context, node Node) (map[uint64]uint64, error) {
	const query = `
		SELECT c.device_id, c.epoch
		FROM device_connections c
		JOIN gateway_nodes n ON n.id = c.node_id
		JOIN devices d ON d.id = c.device_id AND d.tenant_id = c.tenant_id AND d.app_id = c.app_id
		WHERE c.node_id = ? AND c.disconnected_at IS NULL AND c.epoch = d.connection_epoch
			AND n.stopped_at IS NULL AND n.lease_until > UTC_TIMESTAMP(6)
			AND d.credential_version = c.credential_version AND d.revoked_at IS NULL AND d.expires_at > UTC_TIMESTAMP(6)`
	rows, err := s.db.QueryContext(ctx, query, node.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := make(map[uint64]uint64)
	for rows.Next() {
		var deviceID, epoch uint64
		if err := rows.Scan(&deviceID, &epoch); err != nil {
			return nil, err
		}
		live[deviceID] = epoch
	}
	return live, rows.Err()
}

func (s *Store) Presence(ctx context.Context, viewer Session, deviceID uint64) (Presence, error) {
	const query = `
		SELECT c.epoch, c.node_id, n.node_name, c.connected_at, c.disconnected_at,
			c.close_reason, GREATEST(c.connected_at, n.last_heartbeat_at),
			CASE
				WHEN d.revoked_at IS NOT NULL THEN 'session_revoked'
				WHEN d.expires_at <= UTC_TIMESTAMP(6) THEN 'session_expired'
				WHEN c.epoch IS NULL THEN 'never_connected'
				WHEN c.credential_version <> d.credential_version THEN 'session_replaced'
				WHEN c.disconnected_at IS NOT NULL THEN c.close_reason
				WHEN n.stopped_at IS NOT NULL THEN 'node_stopped'
				WHEN n.lease_until <= UTC_TIMESTAMP(6) THEN 'node_unavailable'
				ELSE 'online'
			END
		FROM devices d
		LEFT JOIN device_connections c ON c.device_id = d.id AND c.epoch = d.connection_epoch
		LEFT JOIN gateway_nodes n ON n.id = c.node_id
		WHERE d.tenant_id = ? AND d.app_id = ? AND d.user_id = ? AND d.id = ?`
	presence := Presence{DeviceID: deviceID}
	var connection Connection
	var epoch, nodeID sql.Null[uint64]
	var name, reason sql.NullString
	var opened, closed, heartbeat sql.NullTime
	err := s.db.QueryRowContext(ctx, query, viewer.TenantID, viewer.AppID, viewer.UserID, deviceID).Scan(
		&epoch, &nodeID, &name, &opened, &closed, &reason, &heartbeat, &presence.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Presence{}, ErrNotFound
	}
	if err != nil {
		return Presence{}, err
	}
	if epoch.Valid {
		connection = Connection{DeviceID: deviceID, Epoch: epoch.V, Node: Node{ID: nodeID.V, Name: name.String},
			ConnectedAt: opened.Time, CloseReason: reason.String}
		presence.LastSeenAt = &heartbeat.Time
		if closed.Valid {
			connection.DisconnectedAt = &closed.Time
			presence.LastSeenAt = &closed.Time
		}
		presence.Connection = &connection
	}
	presence.Online = presence.Status == "online"
	return presence, nil
}

func (s *Store) Connections(ctx context.Context, viewer Session, deviceID, before uint64) ([]Connection, error) {
	const query = `
		SELECT c.epoch, c.node_id, n.node_name, c.connected_at, c.disconnected_at, COALESCE(c.close_reason,'')
		FROM device_connections c
		JOIN devices d ON d.id = c.device_id AND d.tenant_id = c.tenant_id AND d.app_id = c.app_id
		JOIN gateway_nodes n ON n.id = c.node_id
		WHERE c.tenant_id = ? AND c.app_id = ? AND d.user_id = ? AND c.device_id = ?
			AND (? = 0 OR c.epoch < ?)
		ORDER BY c.epoch DESC LIMIT 50`
	rows, err := s.db.QueryContext(ctx, query, viewer.TenantID, viewer.AppID, viewer.UserID, deviceID, before, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := make([]Connection, 0)
	for rows.Next() {
		connection := Connection{DeviceID: deviceID}
		if err := rows.Scan(&connection.Epoch, &connection.Node.ID, &connection.Node.Name, &connection.ConnectedAt,
			&connection.DisconnectedAt, &connection.CloseReason); err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}
