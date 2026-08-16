package api

import (
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/one-search/one-search/backend/internal/model"
    "github.com/one-search/one-search/backend/internal/security"
    "golang.org/x/crypto/bcrypt"
)

const (
    oauthAuthorizationCodeTTL = 5 * time.Minute
    oauthAccessTokenTTL       = time.Hour
    oauthScope                = "search"
)

func (h *Handler) mountOAuth(r chi.Router) {
    r.Get("/.well-known/oauth-authorization-server", h.oauthAuthorizationServerMetadata)
    r.Get("/.well-known/oauth-protected-resource", h.oauthProtectedResourceMetadata)
    r.Get("/.well-known/oauth-protected-resource/mcp", h.oauthProtectedResourceMetadata)
    r.Get("/oauth/authorize", h.oauthAuthorize)
    r.Post("/oauth/login", h.oauthLogin)
    r.Post("/oauth/approve", h.oauthApprove)
    r.Post("/oauth/token", h.oauthToken)
}

func (h *Handler) oauthAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
    issuer := oauthIssuer(r)
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "issuer": issuer,
        "authorization_endpoint": issuer + "/oauth/authorize",
        "token_endpoint": issuer + "/oauth/token",
        "response_types_supported": []string{"code"},
        "grant_types_supported": []string{"authorization_code"},
        "code_challenge_methods_supported": []string{"S256"},
        "token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
        "scopes_supported": []string{oauthScope},
    })
}

func (h *Handler) oauthProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
    issuer := oauthIssuer(r)
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "resource": issuer + "/mcp",
        "authorization_servers": []string{issuer},
        "scopes_supported": []string{oauthScope},
        "bearer_methods_supported": []string{"header"},
    })
}

func (h *Handler) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    clientID := strings.TrimSpace(q.Get("client_id"))
    redirectURI := strings.TrimSpace(q.Get("redirect_uri"))
    challenge := strings.TrimSpace(q.Get("code_challenge"))
    if q.Get("response_type") != "code" || clientID == "" || redirectURI == "" || challenge == "" || q.Get("code_challenge_method") != "S256" || !hasOAuthScope(q.Get("scope")) {
        http.Error(w, "invalid OAuth authorization request", http.StatusBadRequest)
        return
    }
    client, err := h.store.FindOAuthClient(r.Context(), clientID)
    if err != nil || client.Status != "enabled" || !containsString(client.RedirectURIs, redirectURI) {
        http.Error(w, "unknown OAuth client or redirect URI", http.StatusBadRequest)
        return
    }

    if r.Method == http.MethodGet && !h.auth.validSession(oauthSessionToken(r)) {
        renderOAuthLogin(w, client, q)
        return
    }
    if r.Method == http.MethodGet {
        renderOAuthConsent(w, client, q)
        return
    }
}

func (h *Handler) oauthToken(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil {
        oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
        return
    }
    clientID, secret := oauthClientCredentials(r)
    code := strings.TrimSpace(r.Form.Get("code"))
    verifier := strings.TrimSpace(r.Form.Get("code_verifier"))
    redirectURI := strings.TrimSpace(r.Form.Get("redirect_uri"))
    if r.Form.Get("grant_type") != "authorization_code" || clientID == "" || secret == "" || code == "" || verifier == "" || redirectURI == "" {
        oauthError(w, http.StatusBadRequest, "invalid_request", "authorization_code fields are required")
        return
    }
    client, err := h.store.FindOAuthClient(r.Context(), clientID)
    if err != nil || client.Status != "enabled" || bcrypt.CompareHashAndPassword([]byte(client.ClientSecretHash), []byte(secret)) != nil {
        oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
        return
    }
    consumed, err := h.store.ConsumeOAuthAuthorizationCode(r.Context(), client.ID, code, redirectURI, verifier)
    if err != nil {
        oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
        return
    }
    accessToken, err := security.RandomToken("oat_")
    if err != nil {
        oauthError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
        return
    }
    expiresAt := time.Now().Add(oauthAccessTokenTTL)
    if err := h.store.CreateOAuthAccessToken(r.Context(), consumed.ID, consumed.APITokenID, accessToken, expiresAt); err != nil {
        oauthError(w, http.StatusInternalServerError, "server_error", "could not persist access token")
        return
    }
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "access_token": accessToken,
        "token_type": "Bearer",
        "expires_in": int(oauthAccessTokenTTL.Seconds()),
        "scope": oauthScope,
    })
}

func (h *Handler) oauthLogin(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil {
        http.Error(w, "invalid form", http.StatusBadRequest)
        return
    }
    clientID := strings.TrimSpace(r.Form.Get("client_id"))
    client, err := h.store.FindOAuthClient(r.Context(), clientID)
    if err != nil || client.Status != "enabled" {
        http.Error(w, "unknown OAuth client", http.StatusBadRequest)
        return
    }
    token, _, err := h.auth.Login(r.Context(), r.Form.Get("username"), r.Form.Get("password"), clientIP(r))
    if err != nil {
        renderOAuthLoginError(w, client, r.Form, "用户名或密码无效")
        return
    }
    http.SetCookie(w, &http.Cookie{Name: "one_search_oauth_session", Value: token, Path: "/oauth", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: int(h.auth.sessionTTL.Seconds())})
    http.Redirect(w, r, oauthAuthorizeURL(url.Values{
        "response_type": {"code"},
        "client_id": {clientID},
        "redirect_uri": {r.Form.Get("redirect_uri")},
        "state": {r.Form.Get("state")},
        "scope": {oauthScope},
        "code_challenge": {r.Form.Get("code_challenge")},
        "code_challenge_method": {"S256"},
    }), http.StatusFound)
}

func (h *Handler) oauthApprove(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil {
        http.Error(w, "invalid form", http.StatusBadRequest)
        return
    }
    token := oauthSessionToken(r)
    if !h.auth.validSession(token) {
        http.Error(w, "authorization session expired", http.StatusUnauthorized)
        return
    }
    clientID := strings.TrimSpace(r.Form.Get("client_id"))
    redirectURI := strings.TrimSpace(r.Form.Get("redirect_uri"))
    challenge := strings.TrimSpace(r.Form.Get("code_challenge"))
    state := r.Form.Get("state")
    client, err := h.store.FindOAuthClient(r.Context(), clientID)
    if err != nil || client.Status != "enabled" || !containsString(client.RedirectURIs, redirectURI) || challenge == "" {
        http.Error(w, "invalid authorization request", http.StatusBadRequest)
        return
    }
    if r.Form.Get("approve") != "yes" {
        redirectOAuthError(w, r, redirectURI, state, "access_denied")
        return
    }
    code, err := security.RandomToken("oac_")
    if err != nil || h.store.CreateOAuthAuthorizationCode(r.Context(), client.ID, code, challenge, redirectURI) != nil {
        http.Error(w, "could not create authorization code", http.StatusInternalServerError)
        return
    }
    target, _ := url.Parse(redirectURI)
    values := target.Query()
    values.Set("code", code)
    if state != "" { values.Set("state", state) }
    target.RawQuery = values.Encode()
    http.Redirect(w, r, target.String(), http.StatusFound)
}

func (h *Handler) listOAuthClients(w http.ResponseWriter, r *http.Request) {
    clients, err := h.store.ListOAuthClients(r.Context())
    if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
    writeJSON(w, http.StatusOK, map[string]interface{}{"clients": clients})
}

func (h *Handler) createOAuthClient(w http.ResponseWriter, r *http.Request) {
    var req struct { Name string `json:"name"`; RedirectURIs []string `json:"redirect_uris"`; AllowedProviders []string `json:"allowed_providers"` }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { writeError(w, http.StatusBadRequest, "invalid json body"); return }
    client, err := h.store.CreateOAuthClient(r.Context(), req.Name, req.RedirectURIs, req.AllowedProviders)
    if err != nil { writeError(w, http.StatusBadRequest, err.Error()); return }
    h.audit(r, "admin", "oauth_client.create", "oauth_client", strconv.FormatInt(client.ID, 10), map[string]interface{}{"name": client.Name})
    writeJSON(w, http.StatusCreated, client)
}

func (h *Handler) updateOAuthClient(w http.ResponseWriter, r *http.Request) {
    id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err != nil { writeError(w, http.StatusBadRequest, "invalid client id"); return }
    var req struct { Name string `json:"name"`; RedirectURIs []string `json:"redirect_uris"`; AllowedProviders []string `json:"allowed_providers"`; Status string `json:"status"` }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { writeError(w, http.StatusBadRequest, "invalid json body"); return }
    if err := h.store.UpdateOAuthClient(r.Context(), id, req.Name, req.RedirectURIs, req.AllowedProviders, req.Status); err != nil { writeError(w, http.StatusBadRequest, err.Error()); return }
    h.audit(r, "admin", "oauth_client.update", "oauth_client", strconv.FormatInt(id, 10), map[string]interface{}{"status": req.Status})
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) deleteOAuthClient(w http.ResponseWriter, r *http.Request) {
    id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err != nil { writeError(w, http.StatusBadRequest, "invalid client id"); return }
    if err := h.store.DeleteOAuthClient(r.Context(), id); err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
    h.audit(r, "admin", "oauth_client.delete", "oauth_client", strconv.FormatInt(id, 10), nil)
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func oauthIssuer(r *http.Request) string {
    scheme := "https"
    if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "http" { scheme = "http" }
    return scheme + "://" + r.Host
}

func oauthClientCredentials(r *http.Request) (string, string) {
    id, secret, ok := r.BasicAuth()
    if ok { return id, secret }
    return strings.TrimSpace(r.Form.Get("client_id")), strings.TrimSpace(r.Form.Get("client_secret"))
}

func hasOAuthScope(scope string) bool {
    for _, item := range strings.Fields(scope) { if item == oauthScope { return true } }
    return false
}

func containsString(items []string, value string) bool {
    for _, item := range items { if item == value { return true } }
    return false
}

func oauthPKCEChallenge(verifier string) string {
    sum := sha256.Sum256([]byte(verifier))
    return base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthSessionToken(r *http.Request) string {
    if cookie, err := r.Cookie("one_search_oauth_session"); err == nil { return cookie.Value }
    return bearerToken(r)
}

func oauthAuthorizeURL(values url.Values) string { return "/oauth/authorize?" + values.Encode() }

func oauthError(w http.ResponseWriter, status int, code, description string) { writeJSON(w, status, map[string]string{"error": code, "error_description": description}) }

func redirectOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state, code string) {
    target, err := url.Parse(redirectURI); if err != nil { http.Error(w, code, http.StatusBadRequest); return }
    q := target.Query(); q.Set("error", code); if state != "" { q.Set("state", state) }; target.RawQuery = q.Encode(); http.Redirect(w, r, target.String(), http.StatusFound)
}

func renderOAuthLogin(w http.ResponseWriter, client model.OAuthClient, values url.Values) { renderOAuthLoginError(w, client, values, "") }
func renderOAuthLoginError(w http.ResponseWriter, client model.OAuthClient, values url.Values, message string) {
    msg := ""; if message != "" { msg = "<p class=error>" + htmlEscape(message) + "</p>" }
    fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>授权 One Search</title><style>body{font:16px system-ui;background:#f4f7f8;margin:0;color:#172126}.box{max-width:420px;margin:10vh auto;padding:28px;background:white;border:1px solid #d8e0e3;border-radius:8px}input,button{box-sizing:border-box;width:100%%;padding:11px;margin:7px 0;font:inherit}.error{color:#b42318}</style></head><body><main class=box><h1>授权 One Search</h1><p><strong>%s</strong> 请求访问搜索工具。</p>%s<form method="post" action="/oauth/login"><input type=hidden name=client_id value="%s"><input type=hidden name=redirect_uri value="%s"><input type=hidden name=state value="%s"><input type=hidden name=code_challenge value="%s"><input name=username placeholder="管理员用户名" required><input type=password name=password placeholder="管理员密码" required><button type=submit>登录并继续</button></form></main></body></html>`, htmlEscape(client.Name), msg, htmlEscape(values.Get("client_id")), htmlEscape(values.Get("redirect_uri")), htmlEscape(values.Get("state")), htmlEscape(values.Get("code_challenge")))
}
func renderOAuthConsent(w http.ResponseWriter, client model.OAuthClient, values url.Values) {
    fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>确认授权</title><style>body{font:16px system-ui;background:#f4f7f8;margin:0;color:#172126}.box{max-width:420px;margin:10vh auto;padding:28px;background:white;border:1px solid #d8e0e3;border-radius:8px}button{padding:11px;margin:7px 0;font:inherit}.allow{background:#166534;color:white;border:0}.deny{background:white;border:1px solid #b42318;color:#b42318}</style></head><body><main class=box><h1>允许访问？</h1><p><strong>%s</strong> 将获得 One Search 的搜索与网页抓取权限。</p><form method="post" action="/oauth/approve"><input type=hidden name=client_id value="%s"><input type=hidden name=redirect_uri value="%s"><input type=hidden name=state value="%s"><input type=hidden name=code_challenge value="%s"><button class=allow name=approve value=yes type=submit>允许</button><button class=deny name=approve value=no type=submit>拒绝</button></form></main></body></html>`, htmlEscape(client.Name), htmlEscape(values.Get("client_id")), htmlEscape(values.Get("redirect_uri")), htmlEscape(values.Get("state")), htmlEscape(values.Get("code_challenge")))
}
func htmlEscape(value string) string { return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(value) }
