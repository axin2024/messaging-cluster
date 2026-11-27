CREATE TABLE shared_grants (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    grant_version BIGINT UNSIGNED NOT NULL,
    duty_role VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    joined_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    UNIQUE KEY member_in_link (link_id,app_id,user_id),
    KEY active_seats (link_id,app_id,revoked_at,expires_at),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (tenant_id,app_id,user_id) REFERENCES users(tenant_id,app_id,id)
) ENGINE=InnoDB;

CREATE TABLE shared_grant_changes (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    link_id BIGINT UNSIGNED NOT NULL,
    actor_tenant BIGINT UNSIGNED NOT NULL,
    actor_app BIGINT UNSIGNED NOT NULL,
    request_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest BINARY(32) NOT NULL,
    grant_id BIGINT UNSIGNED NOT NULL,
    receipt JSON NOT NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY request_identity (link_id,actor_app,request_key),
    FOREIGN KEY (grant_id) REFERENCES shared_grants(id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (actor_tenant,actor_app) REFERENCES applications(tenant_id,id)
) ENGINE=InnoDB;
