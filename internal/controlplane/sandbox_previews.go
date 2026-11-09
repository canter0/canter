package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// SandboxPreview is a shareable public URL reaching one port on a sandbox.
type SandboxPreview struct {
	ID         string     `json:"id"`
	SandboxID  string     `json:"sandboxId"`
	Port       int        `json:"port"`
	Token      string     `json:"-"`
	URL        string     `json:"url"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	revokedRaw *time.Time
}

const maxPreviewsPerSandbox = 5
const maxRelaysPerSandbox = 64

func previewToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:26], nil
}

// ValidatePreviewDomain rejects a preview domain equal to or beneath the
// public app's registrable domain — previews must never be same-site with
// canter cookies.
func ValidatePreviewDomain(previewDomain, publicURL string) error {
	host := publicURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.Split(strings.TrimSuffix(host, "/"), ":")[0]
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(previewDomain)), ".")
	if d == "" {
		return fmt.Errorf("empty sandbox preview domain")
	}
	h := strings.ToLower(host)
	if d == h || strings.HasSuffix(d, "."+h) || strings.HasSuffix(h, "."+d) {
		return fmt.Errorf("sandbox preview domain %q must not share a site with %q", d, h)
	}
	return nil
}

func previewScan(row pgx.Row) (SandboxPreview, error) {
	var v SandboxPreview
	var kind, id string
	err := row.Scan(&v.ID, &v.SandboxID, &v.Port, &v.Token, &kind, &id, &v.CreatedAt, &v.revokedRaw)
	v.RevokedAt = v.revokedRaw
	v.CreatedBy = kind
	return v, err
}

// CreateSandboxPreview registers a preview for port (cap 5 active).
func (s *Store) CreateSandboxPreview(ctx context.Context, v Sandbox, actor sdk.ActorRef, port int) (SandboxPreview, error) {
	if port < 1024 || port > 65535 {
		return SandboxPreview{}, fmt.Errorf("preview ports must be between 1024 and 65535: %w", ErrConflict)
	}
	token, err := previewToken()
	if err != nil {
		return SandboxPreview{}, err
	}
	id, _ := newID("spv_")
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SandboxPreview{}, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM sandbox_previews WHERE sandbox_id=$1 AND revoked_at IS NULL FOR UPDATE`, v.ID)
	if err != nil {
		return SandboxPreview{}, err
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return SandboxPreview{}, err
	}
	active := len(locked)
	if active >= maxPreviewsPerSandbox {
		return SandboxPreview{}, fmt.Errorf("a sandbox can have at most %d active previews: %w", maxPreviewsPerSandbox, ErrCapacity)
	}
	preview, err := previewScan(tx.QueryRow(ctx, `INSERT INTO sandbox_previews(id,sandbox_id,workspace_id,port,token,created_by_kind,created_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id,sandbox_id,port,token,created_by_kind,created_by_id,created_at,revoked_at`,
		id, v.ID, v.WorkspaceID, port, token, actor.Kind, actor.ID))
	if err != nil {
		return SandboxPreview{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SandboxPreview{}, err
	}
	return preview, nil
}

func (s *Store) SandboxPreviews(ctx context.Context, sandboxID string) ([]SandboxPreview, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,sandbox_id,port,token,created_by_kind,created_by_id,created_at,revoked_at
		FROM sandbox_previews WHERE sandbox_id=$1 AND revoked_at IS NULL ORDER BY created_at`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxPreview
	for rows.Next() {
		v, err := previewScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) RevokeSandboxPreview(ctx context.Context, workspace, previewID string) error {
	result, err := s.pool.Exec(ctx, `UPDATE sandbox_previews SET revoked_at=now() WHERE id=$1 AND workspace_id=$2 AND revoked_at IS NULL`, previewID, workspace)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PreviewForHost resolves <token>.<domain> to its active preview + sandbox.
func (s *Store) PreviewForHost(ctx context.Context, domain, host string) (SandboxPreview, Sandbox, error) {
	suffix := "." + strings.ToLower(domain)
	host = strings.ToLower(strings.Split(host, ":")[0])
	if !strings.HasSuffix(host, suffix) {
		return SandboxPreview{}, Sandbox{}, ErrNotFound
	}
	token := strings.TrimSuffix(host, suffix)
	if token == "" || strings.Contains(token, ".") {
		return SandboxPreview{}, Sandbox{}, ErrNotFound
	}
	var p SandboxPreview
	var sandboxID, workspaceID string
	var createdBy string
	err := s.pool.QueryRow(ctx, `SELECT id,sandbox_id,workspace_id,port,token,created_by_kind,created_at,revoked_at
		FROM sandbox_previews WHERE token=$1 AND revoked_at IS NULL`, token).Scan(
		&p.ID, &sandboxID, &workspaceID, &p.Port, &p.Token, &createdBy, &p.CreatedAt, &p.revokedRaw)
	if err != nil {
		return SandboxPreview{}, Sandbox{}, ErrNotFound
	}
	p.SandboxID = sandboxID
	p.CreatedBy = createdBy
	v, err := s.Sandbox(ctx, workspaceID, sandboxID)
	if err != nil {
		return SandboxPreview{}, Sandbox{}, err
	}
	return p, v, nil
}

// activeRelayCounts bounds concurrent proxied connections per sandbox.
var relayCounts = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

func acquireRelay(sandboxID string) bool {
	relayCounts.Lock()
	defer relayCounts.Unlock()
	if relayCounts.m[sandboxID] >= maxRelaysPerSandbox {
		return false
	}
	relayCounts.m[sandboxID]++
	return true
}

func releaseRelay(sandboxID string) {
	relayCounts.Lock()
	defer relayCounts.Unlock()
	relayCounts.m[sandboxID]--
}

// previewLimiter is a tiny per-preview token bucket (50/s burst 100).
type previewLimiter struct {
	mu      sync.Mutex
	tokens  map[string]float64
	updated map[string]time.Time
}

var previewRate = &previewLimiter{tokens: map[string]float64{}, updated: map[string]time.Time{}}

func (l *previewLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	tokens := l.tokens[key]
	if last, ok := l.updated[key]; ok {
		tokens += now.Sub(last).Seconds() * 50
		if tokens > 100 {
			tokens = 100
		}
	} else {
		tokens = 100
	}
	l.updated[key] = now
	if tokens < 1 {
		l.tokens[key] = tokens
		return false
	}
	l.tokens[key] = tokens - 1
	return true
}

// isPreviewHost reports whether the request targets the preview domain.
func (h *HTTPServer) isPreviewHost(r *http.Request) bool {
	domain := h.config.Sandboxes.PreviewDomain
	if domain == "" {
		return false
	}
	host := strings.ToLower(strings.Split(r.Host, ":")[0])
	return strings.HasSuffix(host, "."+strings.ToLower(domain))
}

// servePreview proxies a preview-host request through DialPort. No Canter
// cookies or Authorization are read or forwarded on this path.
func (h *HTTPServer) servePreview(w http.ResponseWriter, r *http.Request) {
	preview, v, err := h.service.Store.PreviewForHost(r.Context(), h.config.Sandboxes.PreviewDomain, r.Host)
	if err != nil {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	if v.Phase == "asleep" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `<!doctype html><title>Sandbox asleep</title><body style="font-family:system-ui;max-width:32rem;margin:4rem auto"><h1>This sandbox is asleep</h1><p>The preview is unavailable while the sandbox sleeps. Wake it from Canter and try again.</p></body>`)
		return
	}
	if v.Phase != "ready" {
		http.Error(w, "Sandbox is not ready", http.StatusServiceUnavailable)
		return
	}
	if err := sandboxMutationAllowed(v); err != nil {
		writeStoreError(w, err)
		return
	}
	if !previewRate.allow(preview.ID) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if !acquireRelay(v.ID) {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseRelay(v.ID)

	h.service.Store.touchSandbox(r.Context(), v.ID)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("127.0.0.1:%d", preview.Port)
			req.Host = req.URL.Host
			req.Header.Del("Authorization")
			for name := range req.Header {
				if strings.EqualFold(name, "Cookie") {
					req.Header.Del(name)
				}
			}
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				client, err := h.sandboxClient(ctx, v)
				if err != nil {
					return nil, err
				}
				op, err := h.service.Store.beginSandboxMutation(ctx, v, 45*time.Second)
				if err != nil {
					client.Close()
					return nil, err
				}
				stop := context.AfterFunc(op.ctx, client.Close)
				var conn net.Conn
				err = op.run(func(active context.Context) error {
					var err error
					conn, err = client.DialPort(active, preview.Port)
					return err
				})
				stop()
				if err != nil {
					client.Close()
					if conn != nil {
						conn.Close()
					}
					op.close()
					return nil, err
				}
				if op.ctx.Err() != nil {
					client.Close()
					conn.Close()
					op.close()
					return nil, op.ctx.Err()
				}
				owned := &previewConn{Conn: conn, client: client}
				owned.lease = keepSandboxMutation(op, v, func(lctx context.Context) error {
					var live bool
					if err := h.service.Store.pool.QueryRow(lctx,
						`SELECT EXISTS(SELECT 1 FROM sandbox_previews WHERE id=$1 AND sandbox_id=$2 AND revoked_at IS NULL)`,
						preview.ID, v.ID).Scan(&live); err != nil {
						return err
					}
					if !live {
						return ErrNotFound
					}
					return nil
				}, owned.closeRemote)
				context.AfterFunc(r.Context(), func() { _ = owned.Close() })
				return owned, nil
			},
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Set("X-Robots-Tag", "noindex")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Sandbox preview is unreachable", http.StatusBadGateway)
		},
	}
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	proxy.ServeHTTP(w, r)
}

// servePreviewAsk is the Caddy on-demand-TLS "ask" hook; loopback only.
func (h *HTTPServer) servePreviewAsk(w http.ResponseWriter, r *http.Request) {
	host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil || !net.ParseIP(host).IsLoopback() {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	_, _, err := h.service.Store.PreviewForHost(r.Context(), h.config.Sandboxes.PreviewDomain, domain)
	if err != nil {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// sandboxPreviews handles /previews[/{pid}].
func (h *HTTPServer) sandboxPreviews(w http.ResponseWriter, r *http.Request, v Sandbox, p Principal, write bool, parts []string) {
	if h.config.Sandboxes.PreviewDomain == "" {
		writeStoreError(w, ErrNotFound)
		return
	}
	domain := h.config.Sandboxes.PreviewDomain
	urlFor := func(pv SandboxPreview) string { return "https://" + pv.Token + "." + domain }
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			list, err := h.service.Store.SandboxPreviews(r.Context(), v.ID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			out := make([]map[string]any, 0, len(list))
			for _, pv := range list {
				out = append(out, map[string]any{"id": pv.ID, "sandboxId": pv.SandboxID, "port": pv.Port, "url": urlFor(pv), "createdBy": pv.CreatedBy, "createdAt": pv.CreatedAt})
			}
			writeJSON(w, http.StatusOK, map[string]any{"previews": out})
		case http.MethodPost:
			if !write {
				writeStoreError(w, ErrForbidden)
				return
			}
			var input struct {
				Port int `json:"port"`
			}
			if !decodeLimit(w, r, &input, 1024) {
				return
			}
			pv, err := h.service.Store.CreateSandboxPreview(r.Context(), v, p.Actor, input.Port)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, p.Actor, "sandbox.preview_created", v.ID, map[string]any{"preview": pv.ID, "port": pv.Port})
			writeJSON(w, http.StatusCreated, map[string]any{"id": pv.ID, "sandboxId": pv.SandboxID, "port": pv.Port, "url": urlFor(pv), "createdBy": pv.CreatedBy, "createdAt": pv.CreatedAt})
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) == 2 && r.Method == http.MethodDelete {
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := h.service.Store.RevokeSandboxPreview(r.Context(), v.WorkspaceID, parts[1]); err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, p.Actor, "sandbox.preview_revoked", v.ID, map[string]any{"preview": parts[1]})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeStoreError(w, ErrNotFound)
}

// previewConn wraps the sandbox-side connection: positive Read/Write marks
// the live lease active; Close tears down client before raw conn.
type previewConn struct {
	net.Conn
	client sandboxAgentClient
	lease  *sandboxLiveLease
	once   sync.Once
}

func (p *previewConn) Read(b []byte) (int, error) {
	n, err := p.Conn.Read(b)
	if n > 0 && p.lease != nil {
		p.lease.Activity()
	}
	return n, err
}
func (p *previewConn) Write(b []byte) (int, error) {
	n, err := p.Conn.Write(b)
	if n > 0 && p.lease != nil {
		p.lease.Activity()
	}
	return n, err
}
func (p *previewConn) Close() error {
	if p.lease != nil {
		p.lease.Close()
	} else {
		p.closeRemote()
	}
	return nil
}

func (p *previewConn) closeRemote() {
	p.once.Do(func() {
		if p.client != nil {
			p.client.Close()
		}
		_ = p.Conn.Close()
	})
}
