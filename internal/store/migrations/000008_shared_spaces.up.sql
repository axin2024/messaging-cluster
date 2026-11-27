CREATE TABLE shared_spaces (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    host_tenant BIGINT UNSIGNED NOT NULL,
    host_app BIGINT UNSIGNED NOT NULL,
    space_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    title VARCHAR(160) NOT NULL,
    purpose VARCHAR(600) NOT NULL,
    policy_rev BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY space_key_per_host (host_app,space_key),
    UNIQUE KEY owning_space (host_tenant,host_app,id),
    FOREIGN KEY (host_tenant,host_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE partner_invitations (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    space_id BIGINT UNSIGNED NOT NULL,
    peer_tenant BIGINT UNSIGNED NOT NULL,
    peer_app BIGINT UNSIGNED NOT NULL,
    invite_key VARCHAR(80) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy_rev BIGINT UNSIGNED NOT NULL,
    offered_scope JSON NOT NULL,
    seat_limit INT UNSIGNED NOT NULL,
    share_history BOOLEAN NOT NULL,
    accept_before DATETIME(6) NOT NULL,
    project_ends_at DATETIME(6) NOT NULL,
    UNIQUE KEY invite_identity (space_id,invite_key),
    KEY recipient_inbox (peer_tenant,peer_app,id),
    FOREIGN KEY (space_id) REFERENCES shared_spaces(id),
    FOREIGN KEY (peer_tenant,peer_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;

CREATE TABLE partner_links (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    invitation_id BIGINT UNSIGNED NOT NULL UNIQUE,
    space_id BIGINT UNSIGNED NOT NULL,
    peer_tenant BIGINT UNSIGNED NOT NULL,
    peer_app BIGINT UNSIGNED NOT NULL,
    accepted_key VARCHAR(80) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    accepted_scope JSON NOT NULL,
    share_history BOOLEAN NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL DEFAULT 1,
    state ENUM('active','frozen','closed') NOT NULL DEFAULT 'active',
    active_peer BIGINT UNSIGNED GENERATED ALWAYS AS (IF(state='closed',NULL,peer_app)) STORED,
    accepted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY peer_request (peer_tenant,peer_app,accepted_key),
    UNIQUE KEY one_open_relationship (space_id,active_peer),
    FOREIGN KEY (space_id) REFERENCES shared_spaces(id),
    FOREIGN KEY (invitation_id) REFERENCES partner_invitations(id),
    FOREIGN KEY (peer_tenant,peer_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;

CREATE TABLE partner_events (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    actor_tenant BIGINT UNSIGNED NOT NULL,
    actor_app BIGINT UNSIGNED NOT NULL,
    action VARCHAR(40) CHARACTER SET ascii NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY link_timeline (link_id,id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (actor_tenant,actor_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;
