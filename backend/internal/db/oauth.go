package db

import (
    "context"
    "crypto/sha256"
    "encoding/base64"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/jackc/pgx/v4"
    "github.com/one-search/one-search/backend/internal/model"
    "github.com/one-search/one-search/backend/internal/security"
    "golang.org/x/crypto/bcrypt"
)

func (s *Store) ListOAuthClients(ctx context.Context) ([]model.OAuthClient, error) {
    rows, err := s.pool.Query(ctx, `SELECT id,name,client_id,redirect_uris,allowed_providers,status,created_at,updated_at FROM oauth_clients ORDER BY created_at DESC`)
    if err != nil { return nil, err }
    defer rows.Close()
    items := []model.OAuthClient{}
    for rows.Next() {
        var item model.OAuthClient
        if err := rows.Scan(&item.ID, &item.Name, &item.ClientID, &item.RedirectURIs, &item.AllowedProviders, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil { return nil, err }
        items = append(items, item)
    }
    return items, rows.Err()
}

func (s *Store) CreateOAuthClient(ctx context.Context, name string, redirectURIs, allowedProviders []string) (model.OAuthClient, error) {
    name = strings.TrimSpace(name)
    if name == "" || len(redirectURIs) == 0 { return model.OAuthClient{}, fmt.Errorf("name and at least one redirect URI are required") }
    for _, uri := range redirectURIs { if !strings.HasPrefix(uri, "https://") { return model.OAuthClient{}, fmt.Errorf("redirect URIs must use https") } }
    clientID, err := security.RandomToken("osc_"); if err != nil { return model.OAuthClient{}, err }
    clientSecret, err := security.RandomToken("ocs_"); if err != nil { return model.OAuthClient{}, err }
    secretHash, err := bcrypt.GenerateFromPassword([]byte(clientSecret), bcrypt.DefaultCost); if err != nil { return model.OAuthClient{}, err }
    internalToken, err := security.RandomToken("osi_"); if err != nil { return model.OAuthClient{}, err }
    cipher, err := s.crypto.Encrypt(internalToken); if err != nil { return model.OAuthClient{}, err }
    tx, err := s.pool.Begin(ctx); if err != nil { return model.OAuthClient{}, err }; defer tx.Rollback(ctx)
    var apiTokenID int64
    if err := tx.QueryRow(ctx, `INSERT INTO api_tokens (name,token_hash,token_ciphertext,token_prefix,scopes,allowed_providers) VALUES ($1,$2,$3,$4,ARRAY['search'],$5) RETURNING id`, "oauth:"+name, security.HashToken(internalToken), cipher, security.TokenPrefix(internalToken), allowedProviders).Scan(&apiTokenID); err != nil { return model.OAuthClient{}, err }
    var item model.OAuthClient
    if err := tx.QueryRow(ctx, `INSERT INTO oauth_clients (name,client_id,client_secret_hash,api_token_id,redirect_uris,allowed_providers) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id,name,client_id,redirect_uris,allowed_providers,status,created_at,updated_at`, name, clientID, string(secretHash), apiTokenID, redirectURIs, allowedProviders).Scan(&item.ID,&item.Name,&item.ClientID,&item.RedirectURIs,&item.AllowedProviders,&item.Status,&item.CreatedAt,&item.UpdatedAt); err != nil { return model.OAuthClient{}, err }
    if err := tx.Commit(ctx); err != nil { return model.OAuthClient{}, err }
    item.ClientSecret = clientSecret
    return item, nil
}

func (s *Store) UpdateOAuthClient(ctx context.Context, id int64, name string, redirectURIs, allowedProviders []string, status string) error {
    if name == "" || len(redirectURIs) == 0 || (status != "enabled" && status != "disabled") { return fmt.Errorf("invalid OAuth client configuration") }
    tx, err := s.pool.Begin(ctx); if err != nil { return err }; defer tx.Rollback(ctx)
    var apiTokenID int64
    if err := tx.QueryRow(ctx, `UPDATE oauth_clients SET name=$2,redirect_uris=$3,allowed_providers=$4,status=$5,updated_at=now() WHERE id=$1 RETURNING api_token_id`, id, name, redirectURIs, allowedProviders, status).Scan(&apiTokenID); err != nil { return err }
    _, err = tx.Exec(ctx, `UPDATE api_tokens SET allowed_providers=$2,status=$3,updated_at=now() WHERE id=$1`, apiTokenID, allowedProviders, status)
    if err != nil { return err }; return tx.Commit(ctx)
}
func (s *Store) DeleteOAuthClient(ctx context.Context, id int64) error { _, err := s.pool.Exec(ctx, `DELETE FROM oauth_clients WHERE id=$1`, id); return err }
func (s *Store) FindOAuthClient(ctx context.Context, clientID string) (model.OAuthClient, error) {
    var item model.OAuthClient
    err := s.pool.QueryRow(ctx, `SELECT id,name,client_id,client_secret_hash,api_token_id,redirect_uris,allowed_providers,status,created_at,updated_at FROM oauth_clients WHERE client_id=$1`, clientID).Scan(&item.ID,&item.Name,&item.ClientID,&item.ClientSecretHash,&item.APITokenID,&item.RedirectURIs,&item.AllowedProviders,&item.Status,&item.CreatedAt,&item.UpdatedAt)
    return item, err
}
func (s *Store) CreateOAuthAuthorizationCode(ctx context.Context, clientID int64, code, codeChallenge, redirectURI string) error { _, err := s.pool.Exec(ctx, `INSERT INTO oauth_authorization_codes (code_hash,client_id,code_challenge,redirect_uri,expires_at) VALUES ($1,$2,$3,$4,$5)`, security.HashToken(code),clientID,codeChallenge,redirectURI,time.Now().Add(5*time.Minute)); return err }
func (s *Store) ConsumeOAuthAuthorizationCode(ctx context.Context, clientID int64, code, redirectURI, verifier string) (model.OAuthClient, error) {
    tx, err := s.pool.Begin(ctx); if err != nil { return model.OAuthClient{}, err }; defer tx.Rollback(ctx)
    var challenge string
    err = tx.QueryRow(ctx, `UPDATE oauth_authorization_codes SET consumed_at=now() WHERE code_hash=$1 AND client_id=$2 AND redirect_uri=$3 AND consumed_at IS NULL AND expires_at>now() RETURNING code_challenge`, security.HashToken(code),clientID,redirectURI).Scan(&challenge)
    if err != nil { return model.OAuthClient{}, err }
    if pkceChallenge(verifier) != challenge { return model.OAuthClient{}, fmt.Errorf("PKCE verification failed") }
    var item model.OAuthClient
    if err := tx.QueryRow(ctx, `SELECT id,name,client_id,client_secret_hash,api_token_id,redirect_uris,allowed_providers,status,created_at,updated_at FROM oauth_clients WHERE id=$1`,clientID).Scan(&item.ID,&item.Name,&item.ClientID,&item.ClientSecretHash,&item.APITokenID,&item.RedirectURIs,&item.AllowedProviders,&item.Status,&item.CreatedAt,&item.UpdatedAt); err != nil { return model.OAuthClient{}, err }
    if err := tx.Commit(ctx); err != nil { return model.OAuthClient{}, err }; return item,nil
}
func (s *Store) CreateOAuthAccessToken(ctx context.Context, clientID, apiTokenID int64, token string, expiresAt time.Time) error { _, err := s.pool.Exec(ctx, `INSERT INTO oauth_access_tokens (token_hash,client_id,api_token_id,expires_at) VALUES ($1,$2,$3,$4)`,security.HashToken(token),clientID,apiTokenID,expiresAt); return err }
func pkceChallenge(verifier string) string {
    sum := sha256.Sum256([]byte(verifier))
    return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Store) FindOAuthAccessToken(ctx context.Context, token string) (model.APIToken, error) {
    var item model.APIToken
    err := s.pool.QueryRow(ctx, `SELECT a.id,a.name,a.token_hash,a.token_prefix,a.scopes,a.allowed_providers,a.status,a.rate_limit_per_min,a.daily_quota,a.monthly_quota,a.last_used_at,a.usage_count,a.created_at,a.updated_at FROM oauth_access_tokens o JOIN api_tokens a ON a.id=o.api_token_id WHERE o.token_hash=$1 AND o.revoked_at IS NULL AND o.expires_at>now() AND a.status='enabled'`, security.HashToken(token)).Scan(&item.ID,&item.Name,&item.TokenHash,&item.TokenPrefix,&item.Scopes,&item.AllowedProviders,&item.Status,&item.RateLimitPerMin,&item.DailyQuota,&item.MonthlyQuota,&item.LastUsedAt,&item.UsageCount,&item.CreatedAt,&item.UpdatedAt)
    if err == nil { _, _ = s.pool.Exec(ctx, `UPDATE oauth_access_tokens SET last_used_at=now() WHERE token_hash=$1`,security.HashToken(token)) }
    if errors.Is(err, pgx.ErrNoRows) { return model.APIToken{}, err }; return item,err
}
