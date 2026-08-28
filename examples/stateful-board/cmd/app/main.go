package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

var version = "development"

type server struct{ db *pgxpool.Pool }

type user struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Token       string `json:"token,omitempty"`
}

type post struct {
	ID         int64     `json:"id"`
	AuthorID   int64     `json:"authorId"`
	AuthorName string    `json:"authorName"`
	Body       string    `json:"body"`
	Likes      int64     `json:"likes"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func main() {
	databaseURL := os.Getenv("CANTER_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("CANTER_SERVICE_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	s := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"version": version}) })
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/posts", s.createPost)
	mux.HandleFunc("PATCH /api/posts/{id}", s.updatePost)
	mux.HandleFunc("POST /api/posts/{id}/like", s.likePost)
	mux.HandleFunc("GET /api/feed", s.feed)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/stats", s.stats)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("stateful board %s listening on %s", version, port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS posts (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  body TEXT NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS likes (
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, post_id)
);
CREATE INDEX IF NOT EXISTS posts_created_at_idx ON posts(created_at DESC);
CREATE INDEX IF NOT EXISTS posts_body_search_idx ON posts USING gin(to_tsvector('english', body));
`)
	return err
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	var one int
	if err := s.db.QueryRow(r.Context(), "SELECT 1").Scan(&one); err != nil {
		writeJSON(w, 503, map[string]any{"ok": false, "database": false})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "database": true, "version": version})
}

func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, DisplayName string }
	if !decode(w, r, &input) || input.Email == "" || input.DisplayName == "" {
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	defer tx.Rollback(r.Context())
	u := user{Email: strings.ToLower(strings.TrimSpace(input.Email)), DisplayName: strings.TrimSpace(input.DisplayName), Token: token()}
	if err := tx.QueryRow(r.Context(), "INSERT INTO users(email, display_name) VALUES($1,$2) RETURNING id", u.Email, u.DisplayName).Scan(&u.ID); err != nil {
		problem(w, 409, err)
		return
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO sessions(token,user_id) VALUES($1,$2)", u.Token, u.ID); err != nil {
		problem(w, 500, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		problem(w, 500, err)
		return
	}
	writeJSON(w, 201, u)
}

func (s *server) createSession(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email string }
	if !decode(w, r, &input) {
		return
	}
	u := user{Email: strings.ToLower(strings.TrimSpace(input.Email)), Token: token()}
	err := s.db.QueryRow(r.Context(), "SELECT id, display_name FROM users WHERE email=$1", u.Email).Scan(&u.ID, &u.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, 404, err)
		return
	}
	if err != nil {
		problem(w, 500, err)
		return
	}
	if _, err := s.db.Exec(r.Context(), "INSERT INTO sessions(token,user_id) VALUES($1,$2)", u.Token, u.ID); err != nil {
		problem(w, 500, err)
		return
	}
	writeJSON(w, 201, u)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, u)
}

func (s *server) createPost(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var input struct{ Body string }
	if !decode(w, r, &input) || strings.TrimSpace(input.Body) == "" {
		return
	}
	p := post{AuthorID: u.ID, AuthorName: u.DisplayName, Body: strings.TrimSpace(input.Body)}
	err := s.db.QueryRow(r.Context(), "INSERT INTO posts(user_id,body) VALUES($1,$2) RETURNING id,created_at,updated_at", u.ID, p.Body).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		problem(w, 500, err)
		return
	}
	writeJSON(w, 201, p)
}

func (s *server) updatePost(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		problem(w, 400, err)
		return
	}
	var input struct{ Body string }
	if !decode(w, r, &input) || strings.TrimSpace(input.Body) == "" {
		return
	}
	command, err := s.db.Exec(r.Context(), "UPDATE posts SET body=$1,updated_at=now() WHERE id=$2 AND user_id=$3", strings.TrimSpace(input.Body), id, u.ID)
	if err != nil {
		problem(w, 500, err)
		return
	}
	if command.RowsAffected() == 0 {
		problem(w, 404, pgx.ErrNoRows)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "updated": true})
}

func (s *server) likePost(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		problem(w, 400, err)
		return
	}
	_, err = s.db.Exec(r.Context(), "INSERT INTO likes(user_id,post_id) VALUES($1,$2) ON CONFLICT DO NOTHING", u.ID, id)
	if err != nil {
		problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"postId": id, "liked": true})
}

func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT p.id,p.user_id,u.display_name,p.body,count(l.user_id),p.created_at,p.updated_at FROM posts p JOIN users u ON u.id=p.user_id LEFT JOIN likes l ON l.post_id=p.id GROUP BY p.id,u.display_name ORDER BY p.created_at DESC LIMIT 50`)
	if err != nil {
		problem(w, 500, err)
		return
	}
	defer rows.Close()
	posts := make([]post, 0, 50)
	for rows.Next() {
		var p post
		if err := rows.Scan(&p.ID, &p.AuthorID, &p.AuthorName, &p.Body, &p.Likes, &p.CreatedAt, &p.UpdatedAt); err != nil {
			problem(w, 500, err)
			return
		}
		posts = append(posts, p)
	}
	writeJSON(w, 200, posts)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = "board"
	}
	rows, err := s.db.Query(r.Context(), `SELECT p.id,p.user_id,u.display_name,p.body,0,p.created_at,p.updated_at FROM posts p JOIN users u ON u.id=p.user_id WHERE to_tsvector('english',p.body) @@ plainto_tsquery('english',$1) ORDER BY p.created_at DESC LIMIT 25`, query)
	if err != nil {
		problem(w, 500, err)
		return
	}
	defer rows.Close()
	posts := make([]post, 0, 25)
	for rows.Next() {
		var p post
		if err := rows.Scan(&p.ID, &p.AuthorID, &p.AuthorName, &p.Body, &p.Likes, &p.CreatedAt, &p.UpdatedAt); err != nil {
			problem(w, 500, err)
			return
		}
		posts = append(posts, p)
	}
	writeJSON(w, 200, posts)
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	var users, sessions, posts, likes int64
	err := s.db.QueryRow(r.Context(), "SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM sessions),(SELECT count(*) FROM posts),(SELECT count(*) FROM likes)").Scan(&users, &sessions, &posts, &likes)
	if err != nil {
		problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"users": users, "sessions": sessions, "posts": posts, "likes": likes})
}

func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (user, bool) {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var u user
	err := s.db.QueryRow(r.Context(), "SELECT u.id,u.email,u.display_name FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token=$1", value).Scan(&u.ID, &u.Email, &u.DisplayName)
	if err != nil {
		problem(w, 401, err)
		return user{}, false
	}
	return u, true
}

func token() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(value); err != nil {
		problem(w, 400, err)
		return false
	}
	return true
}
func problem(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "detail": err.Error()})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
