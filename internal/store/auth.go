package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

// Token kinds.
const (
	TokenAPI    = "api"    // cpk_… — used by the CLI (and any API client)
	TokenWorker = "worker" // spt_… — used by chiperka-worker
)

type User struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"created_at"`
}

type Token struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

type Project struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	RunCount  int    `json:"run_count"`
	LastRunAt int64  `json:"last_run_at"`
}

// HashToken returns the hex sha256 of a secret token.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// RandomToken returns prefix + 40 random hex characters.
func RandomToken(prefix string) string {
	b := make([]byte, 20)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ── users & sessions ────────────────────────────────────────────────────────

func (s *Store) CountUsers(ctx context.Context) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users(email,password_hash,created_at) VALUES(?,?,?)`, email, passwordHash, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByEmail returns the user and its password hash.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, string, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id,email,created_at,password_hash FROM users WHERE email=?`, email).
		Scan(&u.ID, &u.Email, &u.CreatedAt, &hash)
	if err != nil {
		return nil, "", notFound(err)
	}
	return &u, hash, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,email,created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, passwordHash, id)
	return err
}

func (s *Store) CreateSession(ctx context.Context, userID int64, token string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, HashToken(token), userID, expiresAt)
	return err
}

func (s *Store) GetSessionUser(ctx context.Context, token string) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.created_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, HashToken(token), Now()).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) {
	s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, HashToken(token))
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) {
	s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?`, Now())
}

// ── tokens ──────────────────────────────────────────────────────────────────

// CreateToken generates and stores a new token, returning its id and the plain secret.
func (s *Store) CreateToken(ctx context.Context, name, kind string) (int64, string, error) {
	prefix := "cpk_"
	if kind == TokenWorker {
		prefix = "spt_"
	}
	secret := RandomToken(prefix)
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens(name,kind,token_hash,prefix,created_at) VALUES(?,?,?,?,?)`,
		name, kind, HashToken(secret), secret[:len(prefix)+6], Now())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	return id, secret, nil
}

// LookupToken resolves a plain secret to its token row and bumps last_used_at.
func (s *Store) LookupToken(ctx context.Context, secret string) (*Token, error) {
	var t Token
	err := s.db.QueryRowContext(ctx, `SELECT id,name,kind,prefix,created_at,last_used_at FROM tokens WHERE token_hash=?`, HashToken(secret)).
		Scan(&t.ID, &t.Name, &t.Kind, &t.Prefix, &t.CreatedAt, &t.LastUsedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if now := Now(); now-t.LastUsedAt > 60_000 {
		s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at=? WHERE id=?`, now, t.ID)
	}
	return &t, nil
}

func (s *Store) ListTokens(ctx context.Context, kind string) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,prefix,created_at,last_used_at FROM tokens WHERE kind=? ORDER BY id DESC`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.Prefix, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id=?`, id)
	return err
}

// ── projects ────────────────────────────────────────────────────────────────

const projectCols = `p.id,p.slug,p.name,p.created_at,
	(SELECT COUNT(*) FROM runs r WHERE r.project_id=p.id),
	COALESCE((SELECT MAX(started_at) FROM runs r WHERE r.project_id=p.id),0)`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	if err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.CreatedAt, &p.RunCount, &p.LastRunAt); err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects p ORDER BY p.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) GetProjectBySlug(ctx context.Context, slug string) (*Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.slug=?`, slug))
}

func (s *Store) GetProject(ctx context.Context, id int64) (*Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.id=?`, id))
}

// EnsureProject returns the project with slug, creating it when missing.
func (s *Store) EnsureProject(ctx context.Context, slug, name string) (*Project, error) {
	if name == "" {
		name = slug
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(slug,name,created_at) VALUES(?,?,?) ON CONFLICT(slug) DO NOTHING`, slug, name, Now())
	if err != nil {
		return nil, err
	}
	return s.GetProjectBySlug(ctx, slug)
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET project_id=NULL WHERE project_id=?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
		return err
	})
}
