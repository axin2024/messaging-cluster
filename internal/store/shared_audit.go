package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type SharedAuditEvent struct {
	ID         uint64          `json:"id"`
	Actor      Scope           `json:"actor"`
	Action     string          `json:"action"`
	Epoch      uint64          `json:"grant_epoch"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
}

type sharedAdminLink struct {
	host, peer             Scope
	state                  string
	epoch, projectSeq      uint64
	contentEpoch           uint64
	hostFrozen, peerFrozen bool
	expiresAt              time.Time
	live                   bool
}

func lockSharedAdminLink(ctx context.Context, tx *sql.Tx, actor Scope, link uint64, write bool) (sharedAdminLink, error) {
	var out sharedAdminLink
	lock := " FOR SHARE OF l"
	if write {
		lock = " FOR UPDATE OF l"
	}
	err := tx.QueryRowContext(ctx, `SELECT l.state,l.grant_epoch,l.last_project_seq,l.content_epoch,l.host_frozen,l.peer_frozen,
		s.host_tenant,s.host_app,l.peer_tenant,l.peer_app,i.project_ends_at,i.project_ends_at>UTC_TIMESTAMP(6)
		FROM partner_links l JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
		WHERE l.id=? AND ((s.host_tenant=? AND s.host_app=?) OR (l.peer_tenant=? AND l.peer_app=?))`+lock,
		link, actor.TenantID, actor.AppID, actor.TenantID, actor.AppID).Scan(&out.state, &out.epoch, &out.projectSeq, &out.contentEpoch, &out.hostFrozen, &out.peerFrozen,
		&out.host.TenantID, &out.host.AppID, &out.peer.TenantID, &out.peer.AppID, &out.expiresAt, &out.live)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

func recordSharedAudit(ctx context.Context, tx *sql.Tx, link uint64, actor Scope, action string, epoch uint64, detail any) error {
	var raw []byte
	if detail != nil {
		var err error
		raw, err = json.Marshal(detail)
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO partner_events (link_id,actor_tenant,actor_app,action,grant_epoch,detail) VALUES (?,?,?,?,?,?)`,
		link, actor.TenantID, actor.AppID, action, epoch, raw)
	return err
}

func (s *Store) SharedAudit(ctx context.Context, actor Scope, link, after uint64) ([]SharedAuditEvent, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = lockSharedAdminLink(ctx, tx, actor, link, false); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,actor_tenant,actor_app,action,grant_epoch,detail,occurred_at FROM partner_events
		WHERE link_id=? AND id>? AND ((actor_tenant=? AND actor_app=?) OR action IN ('accepted','frozen','active','closed'))
		ORDER BY id LIMIT 100`, link, after, actor.TenantID, actor.AppID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SharedAuditEvent{}
	for rows.Next() {
		var e SharedAuditEvent
		var raw []byte
		if err = rows.Scan(&e.ID, &e.Actor.TenantID, &e.Actor.AppID, &e.Action, &e.Epoch, &raw, &e.OccurredAt); err != nil {
			return nil, err
		}
		e.Detail = raw
		items = append(items, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
