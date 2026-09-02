package controlplane

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_initial.sql
var initialMigration string

//go:embed migrations/002_initial_deployments.sql
var initialDeploymentsMigration string

//go:embed migrations/003_system_m1_prefixes.sql
var systemM1PrefixesMigration string

//go:embed migrations/004_node_gateway.sql
var nodeGatewayMigration string

//go:embed migrations/005_agent_credential_families.sql
var agentCredentialFamiliesMigration string

//go:embed migrations/006_initial_deployment_retries.sql
var initialDeploymentRetriesMigration string

//go:embed migrations/007_change_approval_capabilities.sql
var changeApprovalCapabilitiesMigration string

//go:embed migrations/008_standing_policies.sql
var standingPoliciesMigration string

//go:embed migrations/009_workspace_usage_caps.sql
var workspaceUsageCapsMigration string

var (
	ErrNotFound      = errors.New("not found")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrDevicePending = errors.New("device authorization pending")
	ErrDeviceDenied  = errors.New("device authorization denied")
	ErrDeviceExpired = errors.New("device authorization expired")
	ErrConflict      = errors.New("conflict")
	ErrCapacity      = errors.New("capacity unavailable")
)

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect control-plane postgres: %w", err)
	}
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}, nil
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ready(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, initialMigration); err != nil {
		return fmt.Errorf("apply control-plane migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('001_initial') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, initialDeploymentsMigration); err != nil {
		return fmt.Errorf("apply initial deployments migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('002_initial_deployments') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, systemM1PrefixesMigration); err != nil {
		return fmt.Errorf("apply System m1 prefix migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('003_system_m1_prefixes') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, nodeGatewayMigration); err != nil {
		return fmt.Errorf("apply node gateway migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('004_node_gateway') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, agentCredentialFamiliesMigration); err != nil {
		return fmt.Errorf("apply agent credential families migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('005_agent_credential_families') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, initialDeploymentRetriesMigration); err != nil {
		return fmt.Errorf("apply initial deployment retries migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('006_initial_deployment_retries') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, changeApprovalCapabilitiesMigration); err != nil {
		return fmt.Errorf("apply Change approval capabilities migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('007_change_approval_capabilities') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, standingPoliciesMigration); err != nil {
		return fmt.Errorf("apply standing policies migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('008_standing_policies') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, workspaceUsageCapsMigration); err != nil {
		return fmt.Errorf("apply workspace usage caps migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('009_workspace_usage_caps') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SeedInvite(ctx context.Context, key, label string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("invite key is required")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO beta_invites(key_hash,label) VALUES($1,$2) ON CONFLICT DO NOTHING`, secretHash(key), label)
	return err
}

func (s *Store) Signup(ctx context.Context, email, password, invite string, requireInvite bool) (Account, Workspace, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return Account{}, Workspace{}, "", err
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return Account{}, Workspace{}, "", err
	}
	accountID, _ := newID("usr_")
	workspaceID, _ := newID("wrk_")
	sessionID, _ := newID("hss_")
	token, _ := newSecret("chs_", 32)
	now := s.now()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Account{}, Workspace{}, "", err
	}
	defer tx.Rollback(ctx)
	if requireInvite {
		result, err := tx.Exec(ctx, `UPDATE beta_invites SET consumed_by=$1,consumed_at=$2 WHERE key_hash=$3 AND consumed_at IS NULL`, accountID, now, secretHash(invite))
		if err != nil {
			return Account{}, Workspace{}, "", err
		}
		if result.RowsAffected() != 1 {
			return Account{}, Workspace{}, "", fmt.Errorf("%w: invalid or consumed beta invite", ErrForbidden)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts(id,email,password_hash,created_at) VALUES($1,$2,$3,$4)`, accountID, email, passwordHash, now); err != nil {
		if strings.Contains(err.Error(), "accounts_email_key") {
			return Account{}, Workspace{}, "", fmt.Errorf("%w: account already exists", ErrConflict)
		}
		return Account{}, Workspace{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces(id,name,created_at) VALUES($1,'default',$2)`, workspaceID, now); err != nil {
		return Account{}, Workspace{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,created_at,updated_at) VALUES($1,500,$2,$2)`, workspaceID, now); err != nil {
		return Account{}, Workspace{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, accountID, workspaceID); err != nil {
		return Account{}, Workspace{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,$4,$5)`, sessionID, accountID, secretHash(token), now, now.Add(7*24*time.Hour)); err != nil {
		return Account{}, Workspace{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, Workspace{}, "", err
	}
	return Account{ID: accountID, Email: email, CreatedAt: now}, Workspace{ID: workspaceID, Name: "default", Revision: 1, Role: "owner"}, token, nil
}

func (s *Store) Signin(ctx context.Context, email, password string) (Account, []Workspace, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return Account{}, nil, "", ErrUnauthorized
	}
	var account Account
	var passwordHash string
	var disabled *time.Time
	err = s.pool.QueryRow(ctx, `SELECT id,email,password_hash,created_at,disabled_at FROM accounts WHERE email=$1`, email).Scan(&account.ID, &account.Email, &passwordHash, &account.CreatedAt, &disabled)
	if err != nil || disabled != nil || !verifyPassword(passwordHash, password) {
		return Account{}, nil, "", ErrUnauthorized
	}
	workspaces, err := s.WorkspacesForAccount(ctx, account.ID)
	if err != nil {
		return Account{}, nil, "", err
	}
	sessionID, _ := newID("hss_")
	token, _ := newSecret("chs_", 32)
	now := s.now()
	_, err = s.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,$4,$5)`, sessionID, account.ID, secretHash(token), now, now.Add(7*24*time.Hour))
	return account, workspaces, token, err
}

func (s *Store) ResolveHuman(ctx context.Context, token string) (Principal, error) {
	var p Principal
	var account Account
	var sessionID string
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.email,a.created_at,hs.id FROM human_sessions hs JOIN accounts a ON a.id=hs.account_id WHERE hs.token_hash=$1 AND hs.revoked_at IS NULL AND hs.expires_at>$2 AND a.disabled_at IS NULL`, secretHash(token), s.now()).Scan(&account.ID, &account.Email, &account.CreatedAt, &sessionID)
	if err != nil {
		return p, ErrUnauthorized
	}
	_, _ = s.pool.Exec(ctx, `UPDATE human_sessions SET last_seen_at=$1 WHERE id=$2 AND last_seen_at<$3`, s.now(), sessionID, s.now().Add(-time.Minute))
	p.Account = &account
	p.Actor = sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: sessionID, DisplayName: account.Email}
	return p, nil
}

func (s *Store) RevokeHumanSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE human_sessions SET revoked_at=$1 WHERE token_hash=$2`, s.now(), secretHash(token))
	return err
}

func (s *Store) WorkspacesForAccount(ctx context.Context, accountID string) ([]Workspace, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.id,w.name,w.revision,m.role FROM memberships m JOIN workspaces w ON w.id=m.workspace_id WHERE m.account_id=$1 ORDER BY w.created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Workspace
	for rows.Next() {
		var item Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.Revision, &item.Role); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Membership(ctx context.Context, accountID, workspaceID string) (string, error) {
	var role string
	if err := s.pool.QueryRow(ctx, `SELECT role FROM memberships WHERE account_id=$1 AND workspace_id=$2`, accountID, workspaceID).Scan(&role); err != nil {
		return "", ErrForbidden
	}
	return role, nil
}

type deviceRequest struct {
	ID             string
	Name           string
	Harness        string
	Authority      Authority
	WorkspaceID    *string
	AuthorizedBy   *string
	InstallationID *string
	ExpiresAt      time.Time
	AuthorizedAt   *time.Time
	ExchangedAt    *time.Time
	DeniedAt       *time.Time
}

func (s *Store) BeginDeviceAuthorization(ctx context.Context, name, harness string, authority Authority, publicURL string) (DeviceAuthorization, error) {
	name, harness = strings.TrimSpace(name), strings.TrimSpace(harness)
	if !validAgentLabel(name, 120) || !validAgentLabel(harness, 60) {
		return DeviceAuthorization{}, fmt.Errorf("agent name and harness are required")
	}
	if authority.ApplyMode == "" {
		authority.ApplyMode = "human-approval-required"
	}
	if authority.ApplyMode != "human-approval-required" {
		return DeviceAuthorization{}, fmt.Errorf("only human-approval-required apply authority is supported")
	}
	id, _ := newID("dev_")
	deviceCode, _ := newSecret("cdc_", 32)
	userCode, _ := newUserCode()
	now := s.now()
	_, err := s.pool.Exec(ctx, `INSERT INTO device_authorizations(id,device_hash,user_code,requested_name,harness,requested_inspect,requested_draft,requested_apply_mode,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, secretHash(deviceCode), userCode, name, harness, authority.Inspect, authority.Draft, authority.ApplyMode, now, now.Add(10*time.Minute))
	if err != nil {
		return DeviceAuthorization{}, err
	}
	return DeviceAuthorization{DeviceCode: deviceCode, UserCode: userCode, VerificationURI: strings.TrimRight(publicURL, "/") + "/onboarding/authorize?code=" + userCode, ExpiresAt: now.Add(10 * time.Minute), IntervalSeconds: 2}, nil
}

func scanDevice(row pgx.Row) (deviceRequest, error) {
	var d deviceRequest
	err := row.Scan(&d.ID, &d.Name, &d.Harness, &d.Authority.Inspect, &d.Authority.Draft, &d.Authority.ApplyMode, &d.WorkspaceID, &d.AuthorizedBy, &d.InstallationID, &d.ExpiresAt, &d.AuthorizedAt, &d.ExchangedAt, &d.DeniedAt)
	return d, err
}

func (s *Store) DeviceByUserCode(ctx context.Context, code string) (deviceRequest, error) {
	d, err := scanDevice(s.pool.QueryRow(ctx, `SELECT id,requested_name,harness,requested_inspect,requested_draft,requested_apply_mode,workspace_id,authorized_by,installation_id,expires_at,authorized_at,exchanged_at,denied_at FROM device_authorizations WHERE user_code=$1`, strings.ToUpper(strings.TrimSpace(code))))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (s *Store) ApproveDevice(ctx context.Context, code, accountID, workspaceID string) (Installation, error) {
	role, err := s.Membership(ctx, accountID, workspaceID)
	if err != nil {
		return Installation{}, err
	}
	if role != "owner" {
		return Installation{}, ErrForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Installation{}, err
	}
	defer tx.Rollback(ctx)
	d, err := scanDevice(tx.QueryRow(ctx, `SELECT id,requested_name,harness,requested_inspect,requested_draft,requested_apply_mode,workspace_id,authorized_by,installation_id,expires_at,authorized_at,exchanged_at,denied_at FROM device_authorizations WHERE user_code=$1 FOR UPDATE`, strings.ToUpper(strings.TrimSpace(code))))
	if err != nil {
		return Installation{}, ErrNotFound
	}
	if s.now().After(d.ExpiresAt) {
		return Installation{}, ErrDeviceExpired
	}
	if d.DeniedAt != nil {
		return Installation{}, ErrDeviceDenied
	}
	if d.AuthorizedAt != nil {
		return Installation{}, ErrConflict
	}
	id, _ := newID("agt_")
	now := s.now()
	installation := Installation{ID: id, WorkspaceID: workspaceID, Name: d.Name, Harness: d.Harness, Authority: d.Authority, CreatedBy: accountID, CreatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,inspect_allowed,draft_allowed,apply_mode,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, workspaceID, d.Name, d.Harness, d.Authority.Inspect, d.Authority.Draft, d.Authority.ApplyMode, accountID, now)
	if err != nil {
		return Installation{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE device_authorizations SET workspace_id=$1,authorized_by=$2,installation_id=$3,authorized_at=$4 WHERE id=$5`, workspaceID, accountID, id, now, d.ID)
	if err != nil {
		return Installation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Installation{}, err
	}
	return installation, nil
}

func (s *Store) DenyDevice(ctx context.Context, code, accountID string) error {
	now := s.now()
	result, err := s.pool.Exec(ctx, `UPDATE device_authorizations SET denied_at=$1,authorized_by=$2 WHERE user_code=$3 AND authorized_at IS NULL AND denied_at IS NULL AND expires_at>$1`, now, accountID, strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) ExchangeDevice(ctx context.Context, deviceCode, clientInstance string) (TokenPair, error) {
	clientInstance, err := validatedClientInstance(clientInstance)
	if err != nil {
		return TokenPair{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenPair{}, err
	}
	defer tx.Rollback(ctx)
	d, err := scanDevice(tx.QueryRow(ctx, `SELECT id,requested_name,harness,requested_inspect,requested_draft,requested_apply_mode,workspace_id,authorized_by,installation_id,expires_at,authorized_at,exchanged_at,denied_at FROM device_authorizations WHERE device_hash=$1 FOR UPDATE`, secretHash(deviceCode)))
	if err != nil {
		return TokenPair{}, ErrUnauthorized
	}
	if s.now().After(d.ExpiresAt) {
		return TokenPair{}, ErrDeviceExpired
	}
	if d.DeniedAt != nil {
		return TokenPair{}, ErrDeviceDenied
	}
	if d.AuthorizedAt == nil || d.InstallationID == nil {
		return TokenPair{}, ErrDevicePending
	}
	if d.ExchangedAt != nil {
		return TokenPair{}, ErrConflict
	}
	installation, err := installationByID(ctx, tx, *d.InstallationID)
	if err != nil {
		return TokenPair{}, err
	}
	pair, err := s.issueAgentTokens(ctx, tx, installation, clientInstance, "", "")
	if err != nil {
		return TokenPair{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE device_authorizations SET exchanged_at=$1 WHERE id=$2`, s.now(), d.ID); err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func installationByID(ctx context.Context, q querier, id string) (Installation, error) {
	var i Installation
	err := q.QueryRow(ctx, `SELECT id,workspace_id,name,harness,inspect_allowed,draft_allowed,apply_mode,created_by,created_at,last_seen_at,revoked_at FROM agent_installations WHERE id=$1`, id).Scan(&i.ID, &i.WorkspaceID, &i.Name, &i.Harness, &i.Authority.Inspect, &i.Authority.Draft, &i.Authority.ApplyMode, &i.CreatedBy, &i.CreatedAt, &i.LastSeenAt, &i.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

func (s *Store) issueAgentTokens(ctx context.Context, tx pgx.Tx, installation Installation, clientInstance, familyID, parentID string) (TokenPair, error) {
	credentialID, _ := newID("acr_")
	if familyID == "" {
		familyID = credentialID
	}
	refresh, _ := newSecret("cr_", 32)
	sessionID, _ := newID("ass_")
	access, _ := newSecret("ca_", 32)
	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_credentials(id,installation_id,refresh_hash,family_id,parent_id,created_at,expires_at) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7)`, credentialID, installation.ID, secretHash(refresh), familyID, parentID, now, now.Add(90*24*time.Hour)); err != nil {
		return TokenPair{}, err
	}
	session := AgentSession{ID: sessionID, InstallationID: installation.ID, ClientInstance: clientInstance, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_sessions(id,installation_id,credential_id,access_hash,client_instance,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$6,$7)`, sessionID, installation.ID, credentialID, secretHash(access), clientInstance, now, session.ExpiresAt); err != nil {
		return TokenPair{}, err
	}
	return TokenPair{AccessToken: access, TokenType: "Bearer", ExpiresIn: 900, RefreshToken: refresh, Installation: installation, Session: session}, nil
}

func (s *Store) RefreshAgent(ctx context.Context, refreshToken, clientInstance string) (TokenPair, error) {
	var err error
	clientInstance, err = validatedClientInstance(clientInstance)
	if err != nil {
		return TokenPair{}, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		pair, err := s.refreshAgentOnce(ctx, refreshToken, clientInstance)
		if !isSerializationFailure(err) {
			return pair, err
		}
	}
	return TokenPair{}, fmt.Errorf("refresh credential transaction remained contended")
}

func validAgentLabel(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.IndexFunc(value, unicode.IsControl) == -1
}

func validatedClientInstance(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !validAgentLabel(value, 200) {
		return "", fmt.Errorf("clientInstance must identify this conversation or process and be at most 200 characters")
	}
	return value, nil
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func (s *Store) refreshAgentOnce(ctx context.Context, refreshToken, clientInstance string) (TokenPair, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenPair{}, err
	}
	defer tx.Rollback(ctx)
	var credentialID, installationID, familyID string
	var rotatedAt, revokedAt *time.Time
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT id,installation_id,family_id,rotated_at,revoked_at,expires_at FROM agent_credentials WHERE refresh_hash=$1 FOR UPDATE`, secretHash(refreshToken)).Scan(&credentialID, &installationID, &familyID, &rotatedAt, &revokedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrUnauthorized
	}
	if err != nil {
		return TokenPair{}, err
	}
	now := s.now()
	if rotatedAt != nil {
		if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at=COALESCE(revoked_at,$1) WHERE family_id=$2`, now, familyID); err != nil {
			return TokenPair{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET ended_at=COALESCE(ended_at,$1) WHERE credential_id IN (SELECT id FROM agent_credentials WHERE family_id=$2)`, now, familyID); err != nil {
			return TokenPair{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenPair{}, err
		}
		return TokenPair{}, ErrUnauthorized
	}
	if revokedAt != nil || !expiresAt.After(now) {
		return TokenPair{}, ErrUnauthorized
	}
	installation, err := installationByID(ctx, tx, installationID)
	if err != nil || installation.RevokedAt != nil {
		return TokenPair{}, ErrUnauthorized
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at=$1,rotated_at=$1 WHERE id=$2`, now, credentialID); err != nil {
		return TokenPair{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET ended_at=COALESCE(ended_at,$1) WHERE credential_id=$2`, now, credentialID); err != nil {
		return TokenPair{}, err
	}
	pair, err := s.issueAgentTokens(ctx, tx, installation, clientInstance, familyID, credentialID)
	if err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

func (s *Store) ResolveAgent(ctx context.Context, accessToken string) (Principal, error) {
	var installation Installation
	var session AgentSession
	err := s.pool.QueryRow(ctx, `SELECT i.id,i.workspace_id,i.name,i.harness,i.inspect_allowed,i.draft_allowed,i.apply_mode,i.created_by,i.created_at,i.last_seen_at,i.revoked_at,s.id,s.client_instance,s.created_at,s.last_seen_at,s.expires_at,s.ended_at FROM agent_sessions s JOIN agent_installations i ON i.id=s.installation_id WHERE s.access_hash=$1 AND s.ended_at IS NULL AND s.expires_at>$2 AND i.revoked_at IS NULL`, secretHash(accessToken), s.now()).Scan(&installation.ID, &installation.WorkspaceID, &installation.Name, &installation.Harness, &installation.Authority.Inspect, &installation.Authority.Draft, &installation.Authority.ApplyMode, &installation.CreatedBy, &installation.CreatedAt, &installation.LastSeenAt, &installation.RevokedAt, &session.ID, &session.ClientInstance, &session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt, &session.EndedAt)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	session.InstallationID = installation.ID
	now := s.now()
	if session.LastSeenAt.Before(now.Add(-30 * time.Second)) {
		_, _ = s.pool.Exec(ctx, `UPDATE agent_sessions SET last_seen_at=$1 WHERE id=$2`, now, session.ID)
		_, _ = s.pool.Exec(ctx, `UPDATE agent_installations SET last_seen_at=$1 WHERE id=$2`, now, installation.ID)
		session.LastSeenAt = now
		installation.LastSeenAt = &now
	}
	return Principal{Actor: sdk.ActorRef{Kind: "agent", ID: installation.ID, SessionID: session.ID, DisplayName: installation.Name}, Installation: &installation, Session: &session, WorkspaceID: installation.WorkspaceID}, nil
}

func (s *Store) ListInstallations(ctx context.Context, workspaceID string) ([]Installation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,name,harness,inspect_allowed,draft_allowed,apply_mode,created_by,created_at,last_seen_at,revoked_at FROM agent_installations WHERE workspace_id=$1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var i Installation
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.Name, &i.Harness, &i.Authority.Inspect, &i.Authority.Draft, &i.Authority.ApplyMode, &i.CreatedBy, &i.CreatedAt, &i.LastSeenAt, &i.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInstallation(ctx context.Context, workspaceID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	result, err := tx.Exec(ctx, `UPDATE agent_installations SET revoked_at=$1 WHERE id=$2 AND workspace_id=$3 AND revoked_at IS NULL`, now, id, workspaceID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at=$1 WHERE installation_id=$2 AND revoked_at IS NULL`, now, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET ended_at=$1 WHERE installation_id=$2 AND ended_at IS NULL`, now, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PutSystem(ctx context.Context, workspaceID string, system sdk.System) (SystemRecord, error) {
	if err := validateCanonicalSystemForWorkspace(workspaceID, system); err != nil {
		return SystemRecord{}, err
	}
	raw, err := json.Marshal(system)
	if err != nil {
		return SystemRecord{}, err
	}
	now := s.now()
	var record SystemRecord
	err = s.pool.QueryRow(ctx, `INSERT INTO systems(workspace_id,name,m1_prefix,contract,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(workspace_id,name) DO UPDATE SET contract=EXCLUDED.contract,revision=systems.revision+1,updated_at=EXCLUDED.updated_at WHERE systems.m1_prefix=EXCLUDED.m1_prefix RETURNING revision,created_at,updated_at`, workspaceID, system.Metadata.Name, system.Spec.M1.Prefix, raw, now).Scan(&record.Revision, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, fmt.Errorf("%w: existing System uses a legacy m1 prefix; migrate its stored objects explicitly before changing the contract", ErrConflict)
	}
	if isUniqueViolation(err) {
		return record, fmt.Errorf("%w: System m1 prefix is already owned by another durable System", ErrConflict)
	}
	if err != nil {
		return record, err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE workspaces SET revision=revision+1 WHERE id=$1`, workspaceID)
	record.WorkspaceID = workspaceID
	record.Contract = system
	return record, nil
}

func (s *Store) ListSystems(ctx context.Context, workspaceID string) ([]SystemRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT revision,m1_prefix,contract,created_at,updated_at FROM systems WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemRecord
	for rows.Next() {
		var r SystemRecord
		var prefix string
		var raw []byte
		if err := rows.Scan(&r.Revision, &prefix, &raw, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r.Contract); err != nil {
			return nil, err
		}
		if err := validateStoredSystemPrefix(prefix, r.Contract); err != nil {
			return nil, err
		}
		r.WorkspaceID = workspaceID
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSystem(ctx context.Context, workspaceID, name string) (SystemRecord, error) {
	var r SystemRecord
	var prefix string
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT revision,m1_prefix,contract,created_at,updated_at FROM systems WHERE workspace_id=$1 AND name=$2`, workspaceID, name).Scan(&r.Revision, &prefix, &raw, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r.Contract); err != nil {
		return r, err
	}
	if err = validateStoredSystemPrefix(prefix, r.Contract); err != nil {
		return r, err
	}
	r.WorkspaceID = workspaceID
	return r, nil
}

func validateStoredSystemPrefix(prefix string, system sdk.System) error {
	if err := system.Validate(); err != nil {
		return fmt.Errorf("stored System contract is invalid: %w", err)
	}
	if prefix != system.Spec.M1.Prefix {
		return fmt.Errorf("stored System m1 prefix metadata does not match its contract")
	}
	return nil
}

func (s *Store) Workspace(ctx context.Context, id string) (Workspace, error) {
	var w Workspace
	err := s.pool.QueryRow(ctx, `SELECT id,name,revision FROM workspaces WHERE id=$1`, id).Scan(&w.ID, &w.Name, &w.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

func (s *Store) Audit(ctx context.Context, workspaceID string, actor sdk.ActorRef, action, subject string, metadata any) error {
	id, _ := newID("evt_")
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,actor_kind,actor_id,session_id,action,subject,metadata,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, workspaceID, actor.Kind, actor.ID, actor.SessionID, action, subject, raw, s.now())
	return err
}
