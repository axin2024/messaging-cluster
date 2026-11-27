package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type SharedChannel struct {
	ID          uint64 `json:"channel_id"`
	LinkID      uint64 `json:"link_id"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Audience    string `json:"audience"`
	HistoryFrom string `json:"history_from"`
}

type sharedAccess struct {
	SharedChannel
	floor, epoch, version, projectSeq, lastSeq uint64
	cursorKey                                  []byte
	host                                       bool
	retentionDays                              int
}

func (s *Store) CreateSharedChannel(ctx context.Context, host Scope, link uint64, input SharedChannel) (SharedChannel, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SharedChannel{}, err
	}
	defer tx.Rollback()
	var state string
	var valid, history bool
	err = tx.QueryRowContext(ctx, `SELECT l.state, i.project_ends_at>UTC_TIMESTAMP(6),l.share_history
		FROM partner_links l JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
		WHERE l.id=? AND s.host_tenant=? AND s.host_app=? FOR UPDATE OF l`, link, host.TenantID, host.AppID).Scan(&state, &valid, &history)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedChannel{}, ErrNotFound
	}
	if err != nil {
		return SharedChannel{}, err
	}
	if state != "active" || !valid || (input.HistoryFrom == "beginning" && input.Audience == "shared" && !history) {
		return SharedChannel{}, ErrConflict
	}
	var key [32]byte
	if _, err = rand.Read(key[:]); err != nil {
		return SharedChannel{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_channels (link_id,channel_key,title,audience,history_from,cursor_key)
		VALUES (?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=id`, link, input.Key, input.Title, input.Audience, input.HistoryFrom, key[:])
	if err != nil {
		return SharedChannel{}, err
	}
	var channel SharedChannel
	err = tx.QueryRowContext(ctx, `SELECT id,link_id,channel_key,title,audience,history_from FROM shared_channels WHERE link_id=? AND channel_key=?`,
		link, input.Key).Scan(&channel.ID, &channel.LinkID, &channel.Key, &channel.Title, &channel.Audience, &channel.HistoryFrom)
	if err != nil {
		return SharedChannel{}, err
	}
	input.ID, input.LinkID = channel.ID, channel.LinkID
	if input != channel {
		return SharedChannel{}, ErrPayloadConflict
	}
	return channel, tx.Commit()
}

const sharedChannelJoins = ` JOIN partner_links l ON l.id=c.link_id
	JOIN shared_spaces s ON s.id=l.space_id JOIN partner_invitations i ON i.id=l.invitation_id
	JOIN shared_grants g ON g.link_id=l.id AND g.tenant_id=? AND g.app_id=? AND g.user_id=?`

const sharedChannelAuthority = ` WHERE ((s.host_tenant=g.tenant_id AND s.host_app=g.app_id) OR
	(c.audience='shared' AND l.peer_tenant=g.tenant_id AND l.peer_app=g.app_id AND 'project:read' MEMBER OF(l.accepted_scope)))
	AND l.state!='closed' AND i.project_ends_at>UTC_TIMESTAMP(6) AND g.revoked_at IS NULL AND g.expires_at>UTC_TIMESTAMP(6)`

const sharedHistoryFloor = `IF(c.history_from='beginning' AND (c.audience='host' OR l.share_history),1,g.first_project_seq)`

func (s *Store) ListSharedChannels(ctx context.Context, session Session, link, after uint64) ([]SharedChannel, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.link_id,c.channel_key,c.title,c.audience,c.history_from FROM shared_channels c`+sharedChannelJoins+sharedChannelAuthority+
		` AND c.link_id=? AND c.id>? ORDER BY c.id LIMIT 100`, session.TenantID, session.AppID, session.UserID, link, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := []SharedChannel{}
	for rows.Next() {
		var item SharedChannel
		if err = rows.Scan(&item.ID, &item.LinkID, &item.Key, &item.Title, &item.Audience, &item.HistoryFrom); err != nil {
			return nil, err
		}
		channels = append(channels, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return channels, tx.Commit()
}

// Lock the relationship before reading grants and hold it until the payload read
// or write finishes. Grant changes and relationship closure use the same lock.
func lockSharedAccess(ctx context.Context, tx *sql.Tx, session Session, channel uint64, write bool) (sharedAccess, error) {
	var a sharedAccess
	var link uint64
	err := tx.QueryRowContext(ctx, `SELECT link_id FROM shared_channels WHERE id=?`, channel).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	lock := " FOR SHARE"
	if write {
		lock = " FOR UPDATE"
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM partner_links WHERE id=?`+lock, link).Scan(&link); err != nil {
		return a, err
	}
	var canWrite bool
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.link_id,c.channel_key,c.title,c.audience,c.history_from,c.cursor_key,c.last_seq,
		l.grant_epoch,l.last_project_seq,g.grant_version,`+sharedHistoryFloor+`,
		s.host_app=g.app_id,i.retention_days,l.state='active' AND (s.host_app=g.app_id OR 'thread:write' MEMBER OF(l.accepted_scope)) FROM shared_channels c`+sharedChannelJoins+sharedChannelAuthority+` AND c.id=?`,
		session.TenantID, session.AppID, session.UserID, channel).Scan(&a.ID, &a.LinkID, &a.Key, &a.Title, &a.Audience,
		&a.HistoryFrom, &a.cursorKey, &a.lastSeq, &a.epoch, &a.projectSeq, &a.version, &a.floor, &a.host, &a.retentionDays, &canWrite)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err == nil && write && !canWrite {
		return a, ErrForbidden
	}
	return a, err
}

type SharedMessageInput struct {
	ClientID     string   `json:"client_id"`
	Body         string   `json:"body"`
	Root         *uint64  `json:"root_seq,omitempty"`
	Quote        *uint64  `json:"quote_seq,omitempty"`
	QuoteChannel *uint64  `json:"quote_channel_id,omitempty"`
	Attachments  []uint64 `json:"attachments,omitempty"`
	Mentions     []uint64 `json:"mention_users,omitempty"`
}

type SharedMessage struct {
	Seq          uint64       `json:"seq"`
	SenderTenant uint64       `json:"sender_tenant"`
	SenderUser   uint64       `json:"sender_user"`
	Body         string       `json:"body"`
	Root         *uint64      `json:"root_seq,omitempty"`
	Quote        *uint64      `json:"quote_seq,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	Attachments  []Attachment `json:"attachments,omitempty"`
}

func liveSharedMessage(alias string) string {
	return alias + ".hidden_at IS NULL AND (" + alias + ".retained_until IS NULL OR " + alias + ".retained_until>UTC_TIMESTAMP(6))"
}

func visibleSharedMessage(floor string) string {
	return liveSharedMessage("m") + ` AND m.project_seq>=` + floor + ` AND (m.root_seq IS NULL OR EXISTS (
	SELECT 1 FROM shared_messages root WHERE root.channel_id=m.channel_id AND root.seq=m.root_seq AND ` +
		liveSharedMessage("root") + ` AND root.project_seq>=` + floor + `))`
}

func checkSharedReference(ctx context.Context, tx *sql.Tx, a sharedAccess, seq uint64, root bool) error {
	filter := ""
	if root {
		filter = " AND m.root_seq IS NULL"
	}
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT m.seq FROM shared_messages m WHERE m.channel_id=? AND m.seq=? AND `+visibleSharedMessage("?")+filter,
		a.ID, seq, a.floor, a.floor).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) SendSharedMessage(ctx context.Context, session Session, channel uint64, input SharedMessageInput) (uint64, error) {
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, true)
	if err != nil {
		return 0, err
	}
	seq, err := appendSharedMessage(ctx, tx, session, a, input, nil)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func appendSharedMessage(ctx context.Context, tx *sql.Tx, session Session, a sharedAccess, input SharedMessageInput, origin *LegacyPublication) (uint64, error) {
	channel := a.ID
	if input.QuoteChannel != nil && *input.QuoteChannel != channel {
		return 0, ErrNotFound
	}
	input.QuoteChannel = nil
	if input.Root != nil {
		if err := checkSharedReference(ctx, tx, a, *input.Root, true); err != nil {
			return 0, err
		}
	}
	if input.Quote != nil {
		if err := checkSharedReference(ctx, tx, a, *input.Quote, false); err != nil {
			return 0, err
		}
	}
	raw, err := json.Marshal(struct {
		SharedMessageInput
		Source *LegacyPublication `json:"legacy_source,omitempty"`
	}{input, origin})
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(raw)
	var existing uint64
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT seq,client_digest FROM shared_messages WHERE channel_id=? AND sender_app=? AND sender_user=? AND client_id=?`,
		channel, session.AppID, session.UserID, input.ClientID).Scan(&existing, &prior)
	if err == nil {
		if !bytes.Equal(prior, digest[:]) {
			return 0, ErrPayloadConflict
		}
		if err = checkSharedReference(ctx, tx, a, existing, false); err != nil {
			return 0, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	seq, order := a.lastSeq+1, a.projectSeq+1
	var rootDeadline sql.NullTime
	if input.Root != nil && a.retentionDays != 0 {
		err = tx.QueryRowContext(ctx, `SELECT retained_until FROM shared_messages WHERE channel_id=? AND seq=?`, channel, *input.Root).Scan(&rootDeadline)
		if err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_messages
		(channel_id,seq,project_seq,sender_tenant,sender_app,sender_user,client_id,client_digest,body,root_seq,quote_seq,created_at,retained_until)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6),
		IF(?=0,NULL,LEAST(TIMESTAMPADD(DAY,?,UTC_TIMESTAMP(6)),COALESCE(?,'9999-12-31 00:00:00'))))`,
		channel, seq, order, session.TenantID, session.AppID, session.UserID, input.ClientID, digest[:], input.Body, input.Root, input.Quote,
		a.retentionDays, a.retentionDays, rootDeadline)
	if err != nil {
		return 0, err
	}
	if err = addSharedFiles(ctx, tx, session, channel, seq, input.Attachments); err != nil {
		return 0, err
	}
	if err = addSharedMentions(ctx, tx, a, seq, input.Root, input.Mentions); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_search_index (link_id,project_seq,channel_id,seq,root_seq,body,purge_after)
		SELECT l.id,m.project_seq,m.channel_id,m.seq,m.root_seq,m.body,LEAST(i.project_ends_at,COALESCE(m.retained_until,i.project_ends_at))
		FROM partner_links l JOIN partner_invitations i ON i.id=l.invitation_id
		JOIN shared_messages m ON m.channel_id=? AND m.seq=? WHERE l.id=?`, channel, seq, a.LinkID)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE shared_channels SET last_seq=? WHERE id=?`, seq, channel); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE partner_links SET last_project_seq=? WHERE id=?`, order, a.LinkID); err != nil {
		return 0, err
	}
	if origin != nil {
		if err = recordSharedOrigin(ctx, tx, session, a, seq, *origin); err != nil {
			return 0, err
		}
	}
	return seq, nil
}
