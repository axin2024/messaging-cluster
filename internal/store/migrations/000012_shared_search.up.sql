ALTER TABLE partner_links ADD COLUMN search_key BINARY(32) NULL;
UPDATE partner_links SET search_key=RANDOM_BYTES(32);
ALTER TABLE partner_links MODIFY COLUMN search_key BINARY(32) NOT NULL;

CREATE TABLE shared_search_index (
    link_id BIGINT UNSIGNED NOT NULL,
    project_seq BIGINT UNSIGNED NOT NULL,
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    root_seq BIGINT UNSIGNED NULL,
    body TEXT NOT NULL,
    purge_after DATETIME(6) NOT NULL,
    PRIMARY KEY (link_id,project_seq),
    UNIQUE KEY indexed_message (channel_id,seq),
    KEY indexed_thread (channel_id,root_seq),
    KEY pending_purges (purge_after),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (channel_id,seq) REFERENCES shared_messages(channel_id,seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO shared_search_index (link_id,project_seq,channel_id,seq,root_seq,body,purge_after)
SELECT l.id,m.project_seq,m.channel_id,m.seq,m.root_seq,m.body,i.project_ends_at
FROM shared_messages m JOIN shared_channels c ON c.id=m.channel_id JOIN partner_links l ON l.id=c.link_id
JOIN partner_invitations i ON i.id=l.invitation_id
WHERE m.hidden_at IS NULL AND l.state!='closed' AND i.project_ends_at>UTC_TIMESTAMP(6)
AND (m.root_seq IS NULL OR EXISTS (SELECT 1 FROM shared_messages r WHERE r.channel_id=m.channel_id AND r.seq=m.root_seq AND r.hidden_at IS NULL));

CREATE TABLE shared_message_hides (
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    actor_tenant BIGINT UNSIGNED NOT NULL,
    actor_app BIGINT UNSIGNED NOT NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (channel_id,seq),
    FOREIGN KEY (channel_id,seq) REFERENCES shared_messages(channel_id,seq),
    FOREIGN KEY (actor_tenant,actor_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;
