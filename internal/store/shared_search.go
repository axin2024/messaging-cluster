package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

type SharedSearchHit struct {
	ChannelID uint64    `json:"channel_id"`
	Title     string    `json:"channel_title"`
	Seq       uint64    `json:"seq"`
	Snippet   string    `json:"snippet"`
	CreatedAt time.Time `json:"created_at"`
}

type SharedSearchPage struct {
	Items     []SharedSearchHit `json:"items"`
	Cursor    string            `json:"search_cursor"`
	HasMore   bool              `json:"has_more"`
	PolicyRev uint64            `json:"policy_rev"`
}

func (s *Store) SearchSharedMessages(ctx context.Context, session Session, link uint64, query, token string, limit int) (SharedSearchPage, error) {
	var page SharedSearchPage
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var access sharedAccess
	access.LinkID = link
	err = tx.QueryRowContext(ctx, `SELECT grant_epoch,search_key FROM partner_links WHERE id=? FOR SHARE`, link).Scan(&access.epoch, &access.cursorKey)
	if errors.Is(err, sql.ErrNoRows) {
		return page, ErrNotFound
	}
	if err != nil {
		return page, err
	}
	err = tx.QueryRowContext(ctx, `SELECT g.grant_version,s.policy_rev FROM partner_links l
		JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
		JOIN shared_grants g ON g.link_id=l.id AND g.tenant_id=? AND g.app_id=? AND g.user_id=?
		WHERE l.id=? AND l.state!='closed' AND i.project_ends_at>UTC_TIMESTAMP(6) AND g.revoked_at IS NULL AND g.expires_at>UTC_TIMESTAMP(6)
		AND ((s.host_tenant=g.tenant_id AND s.host_app=g.app_id) OR
		(l.peer_tenant=g.tenant_id AND l.peer_app=g.app_id AND 'project:read' MEMBER OF(l.accepted_scope)))`,
		session.TenantID, session.AppID, session.UserID, link).Scan(&access.version, &page.PolicyRev)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedSearchPage{}, ErrNotFound
	}
	if err != nil {
		return SharedSearchPage{}, err
	}
	codec, err := sharedPositionCodec(access.cursorKey)
	if err != nil {
		return SharedSearchPage{}, err
	}
	hash := sha256.Sum256([]byte(query))
	origin := append(sharedCursorScope(session, access, 0), hash[:]...)
	after, err := readSharedPosition(codec, origin, token)
	if err != nil {
		return SharedSearchPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.channel_id,c.title,m.seq,
		SUBSTRING(m.body,GREATEST(CAST(LOCATE(?,m.body) AS SIGNED)-48,1),160),m.created_at,si.project_seq
		FROM shared_search_index si JOIN shared_channels c ON c.id=si.channel_id
		JOIN shared_messages m ON m.channel_id=si.channel_id AND m.seq=si.seq`+sharedChannelJoins+sharedChannelAuthority+
		` AND l.id=? AND si.link_id=l.id AND si.project_seq>? AND LOCATE(?,si.body)>0 AND `+visibleSharedMessage(sharedHistoryFloor)+
		` ORDER BY si.project_seq LIMIT ?`, query, session.TenantID, session.AppID, session.UserID, link, after, query, limit+1)
	if err != nil {
		return SharedSearchPage{}, err
	}
	defer rows.Close()
	page.Items = make([]SharedSearchHit, 0, limit)
	for rows.Next() {
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		var hit SharedSearchHit
		if err = rows.Scan(&hit.ChannelID, &hit.Title, &hit.Seq, &hit.Snippet, &hit.CreatedAt, &after); err != nil {
			return SharedSearchPage{}, err
		}
		page.Items = append(page.Items, hit)
	}
	if err = rows.Err(); err != nil {
		return SharedSearchPage{}, err
	}
	if err = rows.Close(); err != nil {
		return SharedSearchPage{}, err
	}
	if err = recordSharedDevice(ctx, tx, session, access); err != nil {
		return SharedSearchPage{}, err
	}
	page.Cursor = writeSharedPosition(codec, origin, after)
	return page, tx.Commit()
}

func (s *Store) HideSharedMessage(ctx context.Context, actor Scope, link, channel, seq uint64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	authority, err := lockSharedAdminLink(ctx, tx, actor, link, true)
	if err != nil {
		return err
	}
	var hidden sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT m.hidden_at FROM shared_messages m JOIN shared_channels c ON c.id=m.channel_id
		WHERE c.link_id=? AND m.channel_id=? AND m.seq=? AND m.sender_tenant=? AND m.sender_app=?`,
		link, channel, seq, actor.TenantID, actor.AppID).Scan(&hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if hidden.Valid {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE shared_messages SET body='',hidden_at=UTC_TIMESTAMP(6) WHERE channel_id=? AND seq=?`, channel, seq); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE partner_links SET content_epoch=content_epoch+1 WHERE id=?`, link); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE shared_search_index SET purge_after=LEAST(purge_after,UTC_TIMESTAMP(6))
		WHERE channel_id=? AND (seq=? OR root_seq=?)`, channel, seq, seq); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_message_hides (channel_id,seq,actor_tenant,actor_app) VALUES (?,?,?,?)`,
		channel, seq, actor.TenantID, actor.AppID)
	if err != nil {
		return err
	}
	if err = recordSharedAudit(ctx, tx, link, actor, "message_hidden", authority.epoch, map[string]uint64{"channel_id": channel, "seq": seq}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PruneSharedIndex(ctx context.Context) error {
	const batch = 1000
	for {
		result, err := s.db.ExecContext(ctx, `DELETE FROM shared_search_index WHERE purge_after<=UTC_TIMESTAMP(6) ORDER BY purge_after LIMIT ?`, batch)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count < batch {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE shared_messages SET body='',hidden_at=retained_until
		WHERE retained_until<=UTC_TIMESTAMP(6) AND hidden_at IS NULL ORDER BY retained_until LIMIT ?`, batch)
	return err
}
