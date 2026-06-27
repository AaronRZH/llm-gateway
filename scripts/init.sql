-- 用量记录表（可选，用于持久化官网同步数据）
CREATE TABLE IF NOT EXISTS usage_records (
    id              SERIAL PRIMARY KEY,
    request_id      VARCHAR(64) NOT NULL,
    virtual_model   VARCHAR(64) NOT NULL,
    real_model      VARCHAR(64) NOT NULL,
    provider        VARCHAR(32) NOT NULL,
    input_tokens    INT DEFAULT 0,
    output_tokens   INT DEFAULT 0,
    total_tokens    INT DEFAULT 0,
    estimated_input INT DEFAULT 0,
    estimated_output INT DEFAULT 0,
    official_input  INT DEFAULT 0,
    official_output INT DEFAULT 0,
    created_at      TIMESTAMP DEFAULT NOW(),
    synced_at       TIMESTAMP
);

CREATE INDEX idx_usage_request_id ON usage_records(request_id);
CREATE INDEX idx_usage_created_at ON usage_records(created_at);
CREATE INDEX idx_usage_provider ON usage_records(provider);

-- 校准系数表
CREATE TABLE IF NOT EXISTS calibration (
    model       VARCHAR(64) PRIMARY KEY,
    provider    VARCHAR(32) NOT NULL,
    input_ratio FLOAT DEFAULT 1.0,   -- 官方/估算 输入比例
    output_ratio FLOAT DEFAULT 1.0,  -- 官方/估算 输出比例
    updated_at  TIMESTAMP DEFAULT NOW()
);
