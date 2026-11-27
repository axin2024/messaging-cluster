package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
)

type SharedPage struct {
	Items []SharedMessage `json:"items"`
	Next  string          `json:"cursor"`
	More  bool            `json:"has_more"`
	Epoch uint64          `json:"grant_epoch"`
}

func sharedCursorScope(session Session, a sharedAccess, thread uint64) []byte {
	values := [...]uint64{session.TenantID, session.AppID, session.UserID, session.DeviceID, session.CredentialVersion,
		a.LinkID, a.ID, a.epoch, a.version, thread}
	scope := make([]byte, 0, 8*len(values))
	for _, v := range values {
		scope = binary.BigEndian.AppendUint64(scope, v)
	}
	return scope
}

func sharedPositionCodec(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func readSharedPosition(codec cipher.AEAD, origin []byte, token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	if len(token) != base64.RawURLEncoding.EncodedLen(8+codec.Overhead()) {
		return 0, ErrSharedCursor
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return 0, ErrSharedCursor
	}
	position, err := codec.Open(nil, nil, data, origin)
	if err != nil || len(position) != 8 {
		return 0, ErrSharedCursor
	}
	return binary.BigEndian.Uint64(position), nil
}

func writeSharedPosition(codec cipher.AEAD, origin []byte, position uint64) string {
	return base64.RawURLEncoding.EncodeToString(codec.Seal(nil, nil, binary.BigEndian.AppendUint64(nil, position), origin))
}

func (s *Store) SharedTimeline(ctx context.Context, session Session, channel, thread uint64, token string, limit int) (SharedPage, error) {
	var page SharedPage
	tx, err := s.beginConversationWrite(ctx, session)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	a, err := lockSharedAccess(ctx, tx, session, channel, false)
	if err != nil {
		return page, err
	}
	if thread != 0 {
		if err = checkSharedReference(ctx, tx, a, thread, true); err != nil {
			return page, err
		}
	}
	codec, err := sharedPositionCodec(a.cursorKey)
	if err != nil {
		return page, err
	}
	origin := sharedCursorScope(session, a, thread)
	after, err := readSharedPosition(codec, origin, token)
	if err != nil {
		return page, err
	}
	query := `SELECT m.seq,m.sender_tenant,m.sender_user,m.body,m.root_seq,
		IF(` + liveSharedMessage("q") + ` AND q.project_seq>=? AND (q.root_seq IS NULL OR
		(` + liveSharedMessage("qr") + ` AND qr.project_seq>=?)),m.quote_seq,NULL),m.created_at
		FROM shared_messages m LEFT JOIN shared_messages q ON q.channel_id=m.channel_id AND q.seq=m.quote_seq
		LEFT JOIN shared_messages qr ON qr.channel_id=q.channel_id AND qr.seq=q.root_seq
		WHERE m.channel_id=? AND m.seq>? AND ` + visibleSharedMessage("?")
	args := []any{a.floor, a.floor, channel, after, a.floor, a.floor}
	if thread != 0 {
		query += " AND (m.seq=? OR m.root_seq=?)"
		args = append(args, thread, thread)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY m.seq LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page.Items = make([]SharedMessage, 0, limit)
	for rows.Next() {
		if len(page.Items) == limit {
			page.More = true
			break
		}
		var m SharedMessage
		if err = rows.Scan(&m.Seq, &m.SenderTenant, &m.SenderUser, &m.Body, &m.Root, &m.Quote, &m.CreatedAt); err != nil {
			return SharedPage{}, err
		}
		page.Items = append(page.Items, m)
		after = m.Seq
	}
	if err = rows.Err(); err != nil {
		return SharedPage{}, err
	}
	if err = rows.Close(); err != nil {
		return SharedPage{}, err
	}
	if err = loadSharedFiles(ctx, tx, channel, page.Items); err != nil {
		return SharedPage{}, err
	}
	if err = recordSharedDevice(ctx, tx, session, a); err != nil {
		return SharedPage{}, err
	}
	page.Next = writeSharedPosition(codec, origin, after)
	page.Epoch = a.epoch
	return page, tx.Commit()
}
