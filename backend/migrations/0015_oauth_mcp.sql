CREATE TABLE IF NOT EXISTS oauth_clients (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    client_id TEXT NOT NULL UNIQUE,
    client_secret_hash TEXT NOT NULL,
    api_token_id BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    redirect_uris TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    allowed_providers TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    code_hash TEXT PRIMARY KEY,
    client_id BIGINT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    code_challenge TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT ARRAY['search']::TEXT[],
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_access_tokens (
    token_hash TEXT PRIMARY KEY,
    client_id BIGINT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    api_token_id BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_oauth_authorization_codes_expiry ON oauth_authorization_codes (expires_at);
CREATE INDEX IF NOT EXISTS idx_oauth_access_tokens_expiry ON oauth_access_tokens (expires_at);
