package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

type SharedSpace struct {
	ID        uint64 `json:"space_id"`
	Host      Scope  `json:"host"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Purpose   string `json:"purpose"`
	PolicyRev uint64 `json:"policy_rev"`
}

type Offer struct {
	PeerTenant    string    `json:"peer_tenant"`
	PeerApp       string    `json:"peer_app"`
	Scope         []string  `json:"scope"`
	SeatLimit     int       `json:"seat_limit"`
	ShareHistory  bool      `json:"share_history"`
	RetentionDays int       `json:"retention_days"`
	AcceptBefore  time.Time `json:"accept_before"`
	ProjectEndsAt time.Time `json:"expires_at"`
}

type Invitation struct {
	ID            uint64    `json:"invitation_id"`
	SpaceID       uint64    `json:"space_id"`
	Peer          Scope     `json:"peer"`
	PolicyRev     uint64    `json:"policy_rev"`
	SeatLimit     int       `json:"seat_limit"`
	Scope         []string  `json:"scope"`
	ShareHistory  bool      `json:"share_history"`
	RetentionDays int       `json:"retention_days"`
	AcceptBefore  time.Time `json:"accept_before"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type PartnerLink struct {
	ID            uint64    `json:"link_id"`
	Invitation    uint64    `json:"invitation_id"`
	SpaceID       uint64    `json:"space_id"`
	GrantEpoch    uint64    `json:"grant_epoch"`
	State         string    `json:"state"`
	Scope         []string  `json:"scope"`
	ShareHistory  bool      `json:"share_history"`
	RetentionDays int       `json:"retention_days"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type IncomingInvitation struct {
	Invitation
	Host       Scope  `json:"host"`
	HostTenant string `json:"host_tenant_key"`
	HostApp    string `json:"host_app_key"`
	Title      string `json:"title"`
	Purpose    string `json:"purpose"`
	Status     string `json:"status"`
}

func (s *Store) IncomingInvitations(ctx context.Context, peer Scope, after uint64) ([]IncomingInvitation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.space_id,i.policy_rev,i.seat_limit,i.offered_scope,i.share_history,i.retention_days,i.accept_before,i.project_ends_at,
		s.host_tenant,s.host_app,t.tenant_key,a.app_key,s.title,s.purpose,CASE WHEN l.state='closed' THEN 'closed' WHEN i.project_ends_at<=UTC_TIMESTAMP(6) THEN 'expired'
		WHEN l.id IS NOT NULL THEN l.state WHEN i.accept_before<=UTC_TIMESTAMP(6) THEN 'expired' ELSE 'pending' END
		FROM partner_invitations i JOIN shared_spaces s ON s.id=i.space_id JOIN tenants t ON t.id=s.host_tenant JOIN applications a ON a.id=s.host_app
		LEFT JOIN partner_links l ON l.invitation_id=i.id
		WHERE i.peer_tenant=? AND i.peer_app=? AND i.id>? ORDER BY i.id LIMIT 50`, peer.TenantID, peer.AppID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IncomingInvitation{}
	for rows.Next() {
		var i IncomingInvitation
		var raw []byte
		i.Peer = peer
		if err := rows.Scan(&i.ID, &i.SpaceID, &i.PolicyRev, &i.SeatLimit, &raw, &i.ShareHistory, &i.RetentionDays, &i.AcceptBefore, &i.ExpiresAt,
			&i.Host.TenantID, &i.Host.AppID, &i.HostTenant, &i.HostApp, &i.Title, &i.Purpose, &i.Status); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &i.Scope); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

type PartnerState struct {
	ID           uint64 `json:"link_id"`
	Epoch        uint64 `json:"grant_epoch"`
	State        string `json:"state"`
	HostFrozen   bool   `json:"host_frozen"`
	PeerFrozen   bool   `json:"peer_frozen"`
	ClosureEpoch uint64 `json:"closure_epoch,omitempty"`
}

func (s *Store) ChangePartnerState(ctx context.Context, actor Scope, id, epoch uint64, state string) (PartnerState, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return PartnerState{}, err
	}
	defer tx.Rollback()
	current, err := lockSharedAdminLink(ctx, tx, actor, id, true)
	if err != nil {
		return PartnerState{}, err
	}
	if (current.state == "closed" && state != "closed") || (!current.live && state == "active") {
		return PartnerState{}, ErrConflict
	}
	result := PartnerState{ID: id, Epoch: current.epoch, State: current.state, HostFrozen: current.hostFrozen, PeerFrozen: current.peerFrozen}
	if current.state == "closed" {
		result.ClosureEpoch = current.epoch
	}
	flag := &result.HostFrozen
	if actor == current.peer {
		flag = &result.PeerFrozen
	}
	unchanged := (state == "closed" && current.state == "closed") ||
		(state != "closed" && *flag == (state == "frozen"))
	if unchanged && (epoch == current.epoch || (current.epoch > 1 && epoch == current.epoch-1)) {
		return result, tx.Commit()
	}
	if epoch != current.epoch {
		return PartnerState{}, ErrConflict
	}
	result.Epoch++
	result.State = state
	if state != "closed" {
		*flag = state == "frozen"
		if result.HostFrozen || result.PeerFrozen {
			result.State = "frozen"
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE partner_links SET state=?,grant_epoch=?,host_frozen=?,peer_frozen=? WHERE id=?",
		result.State, result.Epoch, result.HostFrozen, result.PeerFrozen, id); err != nil {
		return PartnerState{}, err
	}
	if state == "closed" {
		result.ClosureEpoch = result.Epoch
		_, err = tx.ExecContext(ctx, `UPDATE shared_search_index SET purge_after=LEAST(purge_after,UTC_TIMESTAMP(6)) WHERE link_id=?`, id)
		if err != nil {
			return PartnerState{}, err
		}
	}
	if err = recordSharedAudit(ctx, tx, id, actor, state, result.Epoch, result); err != nil {
		return PartnerState{}, err
	}
	return result, tx.Commit()
}

func (s *Store) CreateSpace(ctx context.Context, host Scope, key, title, purpose string) (SharedSpace, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO shared_spaces (host_tenant,host_app,space_key,title,purpose) VALUES (?,?,?,?,?)
		ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`, host.TenantID, host.AppID, key, title, purpose)
	if err != nil {
		return SharedSpace{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return SharedSpace{}, err
	}
	row := SharedSpace{ID: uint64(id), Host: host, Key: key}
	err = s.db.QueryRowContext(ctx, "SELECT title,purpose,policy_rev FROM shared_spaces WHERE host_tenant=? AND host_app=? AND id=?", host.TenantID, host.AppID, id).Scan(&row.Title, &row.Purpose, &row.PolicyRev)
	if err == nil && (row.Title != title || row.Purpose != purpose) {
		return SharedSpace{}, ErrPayloadConflict
	}
	return row, err
}

func readOffer(row *sql.Row) (Invitation, error) {
	var invite Invitation
	var permissions []byte
	err := row.Scan(&invite.ID, &invite.SpaceID, &invite.Peer.TenantID, &invite.Peer.AppID, &invite.PolicyRev, &invite.SeatLimit, &permissions, &invite.ShareHistory, &invite.RetentionDays, &invite.AcceptBefore, &invite.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return invite, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(permissions, &invite.Scope)
	}
	return invite, err
}

const offerColumns = "id,space_id,peer_tenant,peer_app,policy_rev,seat_limit,offered_scope,share_history,retention_days,accept_before,project_ends_at"

func (s *Store) InvitePartner(ctx context.Context, host Scope, space uint64, key string, offer Offer) (Invitation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback()
	var rev uint64
	err = tx.QueryRowContext(ctx, "SELECT policy_rev FROM shared_spaces WHERE host_tenant=? AND host_app=? AND id=? FOR UPDATE", host.TenantID, host.AppID, space).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	var peer Scope
	err = tx.QueryRowContext(ctx, `SELECT a.tenant_id,a.id FROM applications a JOIN tenants t ON t.id=a.tenant_id
		WHERE t.tenant_key=? AND a.app_key=? AND t.shared_enabled`, offer.PeerTenant, offer.PeerApp).Scan(&peer.TenantID, &peer.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	if peer.TenantID == host.TenantID {
		return Invitation{}, ErrConflict
	}
	prior, err := readOffer(tx.QueryRowContext(ctx, "SELECT "+offerColumns+" FROM partner_invitations WHERE space_id=? AND invite_key=?", space, key))
	if err == nil {
		if prior.Peer != peer || prior.SeatLimit != offer.SeatLimit || prior.ShareHistory != offer.ShareHistory || prior.RetentionDays != offer.RetentionDays || !slices.Equal(prior.Scope, offer.Scope) ||
			!prior.AcceptBefore.Equal(offer.AcceptBefore) || !prior.ExpiresAt.Equal(offer.ProjectEndsAt) {
			return Invitation{}, ErrPayloadConflict
		}
		return prior, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return Invitation{}, err
	}
	permissions, err := json.Marshal(offer.Scope)
	if err != nil {
		return Invitation{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO partner_invitations (space_id,peer_tenant,peer_app,invite_key,policy_rev,offered_scope,seat_limit,share_history,retention_days,accept_before,project_ends_at)
		SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE UTC_TIMESTAMP(6)<? AND ?<=?`, space, peer.TenantID, peer.AppID, key, rev, permissions, offer.SeatLimit, offer.ShareHistory, offer.RetentionDays, offer.AcceptBefore, offer.ProjectEndsAt,
		offer.AcceptBefore, offer.AcceptBefore, offer.ProjectEndsAt)
	if err != nil {
		return Invitation{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Invitation{}, err
	}
	if id == 0 {
		return Invitation{}, ErrConflict
	}
	invite, err := readOffer(tx.QueryRowContext(ctx, "SELECT "+offerColumns+" FROM partner_invitations WHERE id=?", id))
	if err != nil {
		return Invitation{}, err
	}
	return invite, tx.Commit()
}

func (s *Store) AcceptPartner(ctx context.Context, peer Scope, space, invitation, rev uint64, key string, permissions []string, history bool, retentionDays int) (PartnerLink, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return PartnerLink{}, err
	}
	defer tx.Rollback()
	offer, err := readOffer(tx.QueryRowContext(ctx, "SELECT "+offerColumns+" FROM partner_invitations WHERE id=? AND space_id=? AND peer_tenant=? AND peer_app=? FOR UPDATE",
		invitation, space, peer.TenantID, peer.AppID))
	if err != nil {
		return PartnerLink{}, err
	}
	if rev != offer.PolicyRev || (history && !offer.ShareHistory) || retentionDays != offer.RetentionDays {
		return PartnerLink{}, ErrConflict
	}
	for _, p := range permissions {
		if !slices.Contains(offer.Scope, p) {
			return PartnerLink{}, ErrForbidden
		}
	}
	link := PartnerLink{SpaceID: space, Invitation: invitation, ExpiresAt: offer.ExpiresAt, RetentionDays: offer.RetentionDays}
	var storedKey string
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT id,accepted_scope,share_history,accepted_key FROM partner_links WHERE invitation_id=?", invitation).
		Scan(&link.ID, &raw, &link.ShareHistory, &storedKey)
	if err == nil {
		if err = json.Unmarshal(raw, &link.Scope); err != nil {
			return PartnerLink{}, err
		}
		if storedKey != key || !slices.Equal(permissions, link.Scope) || history != link.ShareHistory {
			return PartnerLink{}, ErrPayloadConflict
		}
		link.GrantEpoch, link.State = 1, "active"
		return link, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PartnerLink{}, err
	}
	raw, err = json.Marshal(permissions)
	if err != nil {
		return PartnerLink{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO partner_links (invitation_id,space_id,peer_tenant,peer_app,accepted_key,accepted_scope,share_history,search_key)
		SELECT ?,?,?,?,?,?,?,RANDOM_BYTES(32) FROM shared_spaces WHERE id=? AND policy_rev=? AND UTC_TIMESTAMP(6)<?`, invitation, space, peer.TenantID, peer.AppID, key, raw, history, space, rev, offer.AcceptBefore)
	if err != nil {
		return PartnerLink{}, conflict(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return PartnerLink{}, err
	}
	if id == 0 {
		var expired bool
		if err = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)>=?", offer.AcceptBefore).Scan(&expired); err != nil {
			return PartnerLink{}, err
		}
		if expired {
			return PartnerLink{}, ErrInvitationExpired
		}
		return PartnerLink{}, ErrConflict
	}
	link.ID, link.State, link.GrantEpoch, link.Scope, link.ShareHistory = uint64(id), "active", 1, permissions, history
	if err = recordSharedAudit(ctx, tx, link.ID, peer, "accepted", 1, link); err != nil {
		return PartnerLink{}, err
	}
	return link, tx.Commit()
}
