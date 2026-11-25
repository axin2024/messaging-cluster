CREATE TABLE tenants (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY tenant_key (tenant_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE applications (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    admin_digest BINARY(32) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY tenant_app (tenant_id, id),
    UNIQUE KEY tenant_app_key (tenant_id, app_key),
    UNIQUE KEY admin_digest (admin_digest),
    CONSTRAINT applications_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE users (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    username VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(100) NOT NULL,
    password_hash VARBINARY(60) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY app_user (tenant_id, app_id, id),
    UNIQUE KEY app_username (tenant_id, app_id, username),
    CONSTRAINT users_application FOREIGN KEY (tenant_id, app_id) REFERENCES applications (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE devices (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    device_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    token_digest BINARY(32) NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY app_device (tenant_id, app_id, id),
    UNIQUE KEY user_device_key (tenant_id, app_id, user_id, device_key),
    UNIQUE KEY token_digest (token_digest),
    CONSTRAINT devices_user FOREIGN KEY (tenant_id, app_id, user_id) REFERENCES users (tenant_id, app_id, id),
    CONSTRAINT devices_state CHECK ((revoked_at IS NULL) = (token_digest IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
