package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const capacity = 120

var buildVersion = "dev"

type server struct {
	db            *pgxpool.Pool
	confirmations bool
}

type accountInput struct {
	ID string `json:"id"`
}

func main() {
	ctx := context.Background()
	databaseURL := os.Getenv("CANTER_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("CANTER_SERVICE_DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := initialize(ctx, db); err != nil {
		log.Fatal(err)
	}
	s := &server{db: db, confirmations: os.Getenv("ENABLE_CONFIRMATIONS") == "true"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /accounts", s.createAccount)
	mux.HandleFunc("GET /availability", s.availability)
	mux.HandleFunc("POST /claims/{user}", s.claim)
	mux.HandleFunc("DELETE /claims/{user}", s.cancel)
	mux.HandleFunc("POST /claims/{user}/confirm", s.confirm)
	mux.HandleFunc("GET /proof", s.proof)
	mux.HandleFunc("GET /change-proof", s.proof)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("flash allotment %s listening on %s confirmations=%t", buildVersion, port, s.confirmations)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, requestLog(mux)))
}

func initialize(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS allotment_inventory (
  singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  capacity INTEGER NOT NULL CHECK (capacity = 120),
  claimed INTEGER NOT NULL DEFAULT 0 CHECK (claimed >= 0 AND claimed <= capacity)
);
INSERT INTO allotment_inventory(singleton, capacity, claimed)
VALUES (TRUE, 120, 0) ON CONFLICT (singleton) DO NOTHING;
CREATE TABLE IF NOT EXISTS accounts (
  id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 80),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS claims (
  id BIGSERIAL PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES accounts(id),
  active BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  canceled_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_claim_per_user
  ON claims(user_id) WHERE active;
`)
	return err
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": buildVersion})
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" || len(in.ID) > 80 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "valid id required"})
		return
	}
	result, err := s.db.Exec(r.Context(), `INSERT INTO accounts(id) VALUES($1) ON CONFLICT DO NOTHING`, in.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if result.RowsAffected() == 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"id": in.ID})
}

func (s *server) availability(w http.ResponseWriter, r *http.Request) {
	var total, claimed int
	if err := s.db.QueryRow(r.Context(), `SELECT capacity, claimed FROM allotment_inventory WHERE singleton`).Scan(&total, &claimed); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capacity": total, "claimed": claimed, "available": total - claimed})
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	tx, err := s.db.BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var id int64
	err = tx.QueryRow(r.Context(), `INSERT INTO claims(user_id) VALUES($1) RETURNING id`, user).Scan(&id)
	if err != nil {
		if isConflict(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "account missing or user already has an active claim"})
			return
		}
		writeError(w, err)
		return
	}
	result, err := tx.Exec(r.Context(), `UPDATE allotment_inventory SET claimed=claimed+1 WHERE singleton AND claimed < capacity`)
	if err != nil {
		writeError(w, err)
		return
	}
	if result.RowsAffected() != 1 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "sold out"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"claimId": id, "user": user, "active": true})
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), `UPDATE claims SET active=FALSE, canceled_at=now() WHERE user_id=$1 AND active`, r.PathValue("user"))
	if err != nil {
		writeError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no active claim"})
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE allotment_inventory SET claimed=claimed-1 WHERE singleton`); err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": r.PathValue("user"), "active": false})
}

func (s *server) confirm(w http.ResponseWriter, r *http.Request) {
	if !s.confirmations {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "confirmations disabled"})
		return
	}
	result, err := s.db.Exec(r.Context(), `UPDATE claims SET confirmed_at=COALESCE(confirmed_at, now()) WHERE user_id=$1 AND active`, r.PathValue("user"))
	if err != nil {
		writeError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no active claim"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": r.PathValue("user"), "confirmed": true})
}

func (s *server) proof(w http.ResponseWriter, r *http.Request) {
	var total, counter, active, duplicates, confirmed int
	var columnExists, indexExists bool
	err := s.db.QueryRow(r.Context(), `
SELECT i.capacity, i.claimed,
       (SELECT count(*) FROM claims WHERE active),
       (SELECT count(*) FROM (SELECT user_id FROM claims WHERE active GROUP BY user_id HAVING count(*) > 1) d),
       EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='claims' AND column_name='confirmed_at'),
       EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND tablename='claims' AND indexname='active_claim_confirmation_idx')
FROM allotment_inventory i WHERE i.singleton`).Scan(&total, &counter, &active, &duplicates, &columnExists, &indexExists)
	if err != nil {
		writeError(w, err)
		return
	}
	if columnExists {
		if err := s.db.QueryRow(r.Context(), `SELECT count(*) FROM claims WHERE active AND confirmed_at IS NOT NULL`).Scan(&confirmed); err != nil {
			writeError(w, err)
			return
		}
	}
	valid := counter >= 0 && counter <= total && counter == active && duplicates == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": valid, "capacity": total, "inventoryCounter": counter, "activeClaims": active,
		"duplicateActiveUsers": duplicates, "oversold": active > total, "counterMatches": counter == active,
		"confirmationsEnabled": s.confirmations, "confirmationColumn": columnExists,
		"confirmationIndex": indexExists, "confirmedActiveClaims": confirmed, "version": buildVersion,
	})
}

func isConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503" || pgErr.Code == "23514")
}

func writeError(w http.ResponseWriter, err error) {
	log.Printf("request failed: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database operation failed"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		_ = fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, strconv.FormatInt(time.Since(started).Milliseconds(), 10))
	})
}
