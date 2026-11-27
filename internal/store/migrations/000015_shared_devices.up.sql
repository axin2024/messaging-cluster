CREATE TABLE shared_read_devices (
    link_id BIGINT UNSIGNED NOT NULL,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL,
    grant_epoch BIGINT UNSIGNED NOT NULL,
    grant_version BIGINT UNSIGNED NOT NULL,
    authorized_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (link_id,device_id),
    KEY member_devices (link_id,app_id,user_id,authorized_at,device_id),
    FOREIGN KEY (link_id) REFERENCES partner_links(id),
    FOREIGN KEY (tenant_id,app_id,user_id) REFERENCES users(tenant_id,app_id,id),
    FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB;
