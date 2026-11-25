ALTER TABLE devices ADD COLUMN connection_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0;

CREATE TABLE gateway_nodes (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_name VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_heartbeat_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    lease_until DATETIME(6) NOT NULL,
    stopped_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    KEY node_lease (lease_until, id)
) ENGINE=InnoDB;

CREATE TABLE device_connections (
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    epoch BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL,
    connected_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    disconnected_at DATETIME(6) NULL,
    close_reason VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (device_id, epoch),
    KEY connection_device (tenant_id, app_id, device_id, epoch),
    KEY node_open_connections (node_id, disconnected_at, device_id, epoch),
    CONSTRAINT connections_device FOREIGN KEY (tenant_id, app_id, device_id) REFERENCES devices (tenant_id, app_id, id),
    CONSTRAINT connections_node FOREIGN KEY (node_id) REFERENCES gateway_nodes (id),
    CONSTRAINT connection_close CHECK ((disconnected_at IS NULL) = (close_reason IS NULL))
) ENGINE=InnoDB;

CREATE TABLE gateway_tickets (
    token_digest BINARY(32) NOT NULL,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    device_id BIGINT UNSIGNED NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (token_digest),
    KEY ticket_expiry (expires_at),
    CONSTRAINT tickets_device FOREIGN KEY (tenant_id, app_id, device_id) REFERENCES devices (tenant_id, app_id, id)
) ENGINE=InnoDB;
