package store

import (
	"context"
	"database/sql"
	"time"
)

type ProjectLink struct {
	PartnerState
	SpaceID       uint64    `json:"space_id"`
	SpaceTitle    string    `json:"space_title"`
	HostTenant    string    `json:"host_tenant"`
	HostApp       string    `json:"host_app"`
	PeerTenant    string    `json:"peer_tenant"`
	PeerApp       string    `json:"peer_app"`
	PolicyRev     uint64    `json:"policy_rev"`
	SeatLimit     int       `json:"seat_limit"`
	RetentionDays int       `json:"retention_days"`
	ExpiresAt     time.Time `json:"expires_at"`
	Expired       bool      `json:"expired"`
	MySide        string    `json:"my_side"`
}

const projectLinkList = `SELECT l.id,l.grant_epoch,l.state,l.host_frozen,l.peer_frozen,s.id,s.title,ht.tenant_key,ha.app_key,pt.tenant_key,pa.app_key,
	s.policy_rev,i.seat_limit,i.retention_days,i.project_ends_at,i.project_ends_at<=UTC_TIMESTAMP(6),s.host_app FROM partner_links l
	JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
	JOIN tenants ht ON ht.id=s.host_tenant JOIN applications ha ON ha.id=s.host_app
	JOIN tenants pt ON pt.id=l.peer_tenant JOIN applications pa ON pa.id=l.peer_app`

func collectProjectLinks(app uint64, rows *sql.Rows, err error) ([]ProjectLink, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ProjectLink{}
	for rows.Next() {
		var x ProjectLink
		var hostID uint64
		if err = rows.Scan(&x.ID, &x.Epoch, &x.State, &x.HostFrozen, &x.PeerFrozen, &x.SpaceID, &x.SpaceTitle, &x.HostTenant, &x.HostApp,
			&x.PeerTenant, &x.PeerApp, &x.PolicyRev, &x.SeatLimit, &x.RetentionDays, &x.ExpiresAt, &x.Expired, &hostID); err != nil {
			return nil, err
		}
		x.MySide = "partner"
		if hostID == app {
			x.MySide = "host"
		}
		if x.State == "closed" {
			x.ClosureEpoch = x.Epoch
		}
		items = append(items, x)
	}
	return items, rows.Err()
}

func (s *Store) AdminProjectLinks(ctx context.Context, actor Scope, after uint64) ([]ProjectLink, error) {
	rows, err := s.db.QueryContext(ctx, projectLinkList+` WHERE l.id>? AND
		((s.host_tenant=? AND s.host_app=?) OR (l.peer_tenant=? AND l.peer_app=?)) ORDER BY l.id LIMIT 100`,
		after, actor.TenantID, actor.AppID, actor.TenantID, actor.AppID)
	return collectProjectLinks(actor.AppID, rows, err)
}

func (s *Store) MemberProjectLinks(ctx context.Context, session Session, after uint64) ([]ProjectLink, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, projectLinkList+` JOIN shared_grants g ON g.link_id=l.id
		WHERE g.tenant_id=? AND g.app_id=? AND g.user_id=? AND g.revoked_at IS NULL AND g.expires_at>UTC_TIMESTAMP(6)
		AND l.state!='closed' AND i.project_ends_at>UTC_TIMESTAMP(6) AND l.id>? ORDER BY l.id LIMIT 100`,
		session.TenantID, session.AppID, session.UserID, after)
	items, err := collectProjectLinks(session.AppID, rows, err)
	if err != nil {
		return nil, err
	}
	return items, tx.Commit()
}

func (s *Store) AdminSharedSpaces(ctx context.Context, actor Scope, after uint64) ([]SharedSpace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,space_key,title,purpose,policy_rev FROM shared_spaces
		WHERE host_tenant=? AND host_app=? AND id>? ORDER BY id LIMIT 100`, actor.TenantID, actor.AppID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SharedSpace{}
	for rows.Next() {
		var item SharedSpace
		item.Host = actor
		if err = rows.Scan(&item.ID, &item.Key, &item.Title, &item.Purpose, &item.PolicyRev); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type SharedGrantSummary struct {
	ID          uint64     `json:"grant_id"`
	UserID      uint64     `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Version     uint64     `json:"grant_version"`
	Role        string     `json:"duty_role"`
	JoinedAt    time.Time  `json:"joined_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Active      bool       `json:"active"`
}

func (s *Store) AdminSharedGrants(ctx context.Context, actor Scope, link, after uint64) ([]SharedGrantSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	access, err := lockSharedAdminLink(ctx, tx, actor, link, false)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.id,g.user_id,u.display_name,g.grant_version,g.duty_role,g.joined_at,g.expires_at,g.revoked_at,
		g.revoked_at IS NULL AND g.expires_at>UTC_TIMESTAMP(6) FROM shared_grants g
		JOIN users u ON u.id=g.user_id AND u.tenant_id=g.tenant_id AND u.app_id=g.app_id
		WHERE g.link_id=? AND g.tenant_id=? AND g.app_id=? AND g.id>? ORDER BY g.id LIMIT 100`, link, actor.TenantID, actor.AppID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SharedGrantSummary{}
	for rows.Next() {
		var value SharedGrantSummary
		if err = rows.Scan(&value.ID, &value.UserID, &value.DisplayName, &value.Version, &value.Role, &value.JoinedAt,
			&value.ExpiresAt, &value.RevokedAt, &value.Active); err != nil {
			return nil, err
		}
		value.Active = value.Active && access.live && access.state != "closed"
		items = append(items, value)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
