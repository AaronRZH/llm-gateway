-- Migration 001: Create usage_records table
-- Run manually if auto-migration is disabled

CREATE TABLE IF NOT EXISTS usage_records (
    id              BIGSERIAL PRIMARY KEY,
    request_id      VARCHAR(255) NOT NULL,
    virtual_model   VARCHAR(255) NOT NULL,
    real_model      VARCHAR(255) NOT NULL,
    provider        VARCHAR(255) NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    est_input       INTEGER NOT NULL DEFAULT 0,
    est_output      INTEGER NOT NULL DEFAULT 0,
    official_in     INTEGER NOT NULL DEFAULT 0,
    official_out    INTEGER NOT NULL DEFAULT 0,
    api_key         VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (request_id)
);

CREATE INDEX IF NOT EXISTS idx_usage_created_at ON usage_records (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_api_key ON usage_records (api_key);
CREATE INDEX IF NOT EXISTS idx_usage_real_model ON usage_records (real_model);
CREATE INDEX IF NOT EXISTS idx_usage_apikey_model_time ON usage_records (api_key, real_model, created_at DESC);