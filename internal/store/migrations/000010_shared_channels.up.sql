ALTER TABLE partner_links ADD COLUMN last_project_seq BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE shared_grants ADD COLUMN first_project_seq BIGINT UNSIGNED NOT NULL DEFAULT 1;

CREATE TABLE shared_channels (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    channel_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    title VARCHAR(160) NOT NULL,
    audience ENUM('host','shared') NOT NULL,
    history_from ENUM('join','beginning') NOT NULL,
    cursor_key BINARY(32) NOT NULL,
    last_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY channel_in_link (link_id,channel_key),
    KEY list_channels (link_id,id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE shared_messages (
    channel_id BIGINT UNSIGNED NOT NULL,
    seq BIGINT UNSIGNED NOT NULL,
    project_seq BIGINT UNSIGNED NOT NULL,
    sender_tenant BIGINT UNSIGNED NOT NULL,
    sender_app BIGINT UNSIGNED NOT NULL,
    sender_user BIGINT UNSIGNED NOT NULL,
    client_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_digest BINARY(32) NOT NULL,
    body TEXT NOT NULL,
    root_seq BIGINT UNSIGNED NULL,
    quote_seq BIGINT UNSIGNED NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    hidden_at DATETIME(6) NULL,
    PRIMARY KEY (channel_id,seq),
    UNIQUE KEY sender_request (channel_id,sender_app,sender_user,client_id),
    KEY thread_messages (channel_id,root_seq,seq),
    KEY message_quote (channel_id,quote_seq),
    FOREIGN KEY (channel_id) REFERENCES shared_channels(id),
    FOREIGN KEY (sender_tenant,sender_app,sender_user) REFERENCES users(tenant_id,app_id,id),
    FOREIGN KEY (channel_id,root_seq) REFERENCES shared_messages(channel_id,seq),
    FOREIGN KEY (channel_id,quote_seq) REFERENCES shared_messages(channel_id,seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
