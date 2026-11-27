ALTER TABLE partner_links ADD COLUMN content_epoch BIGINT UNSIGNED NOT NULL DEFAULT 1;
ALTER TABLE shared_messages ADD KEY export_sender (channel_id,sender_app,project_seq);

CREATE TABLE shared_exports (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    actor_tenant BIGINT UNSIGNED NOT NULL,
    actor_app BIGINT UNSIGNED NOT NULL,
    request_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest BINARY(32) NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL,
    content_epoch BIGINT UNSIGNED NOT NULL,
    after_seq BIGINT UNSIGNED NOT NULL,
    snapshot_seq BIGINT UNSIGNED NOT NULL,
    through_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
    item_count SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    has_more BOOLEAN NOT NULL DEFAULT FALSE,
    state ENUM('queued','ready','failed','invalidated') NOT NULL DEFAULT 'queued',
    attempts TINYINT UNSIGNED NOT NULL DEFAULT 0,
    last_error VARCHAR(80) CHARACTER SET ascii NULL,
    next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    finished_at DATETIME(6) NULL,
    expires_at DATETIME(6) NOT NULL,
    archive_digest BINARY(32) NULL,
    archive LONGBLOB NULL,
    UNIQUE KEY export_request (link_id,actor_app,request_key),
    KEY pending_exports (state,next_attempt_at,id),
    KEY expire_exports (expires_at),
    KEY organization_exports (link_id,actor_app,id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (actor_tenant,actor_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;

CREATE TABLE shared_export_grants (
    token_digest BINARY(32) PRIMARY KEY,
    export_id BIGINT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    KEY expire_export_grants (expires_at),
    FOREIGN KEY (export_id) REFERENCES shared_exports(id)
) ENGINE=InnoDB;
