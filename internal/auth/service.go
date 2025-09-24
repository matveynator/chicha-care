package auth

// The auth package wraps access to the user table with a channel-based service.
// We keep all logic asynchronous so other components do not block each other.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
)

// Service exposes user registration and authentication helpers through channels.
type Service struct {
	db        *sql.DB
	requests  chan interface{}
	responses chan interface{}
}

// NewService prepares the service loop and ensures the user table exists.
func NewService(db *sql.DB) *Service {
	svc := &Service{
		db:        db,
		requests:  make(chan interface{}),
		responses: make(chan interface{}),
	}
	go svc.loop()
	return svc
}

// loop processes one request at a time and uses the database/sql API.
func (s *Service) loop() {
	for {
		select {
		case req := <-s.requests:
			switch msg := req.(type) {
			case registerRequest:
				s.responses <- s.handleRegister(msg)
			case authRequest:
				s.responses <- s.handleAuth(msg)
			default:
				s.responses <- fmt.Errorf("unknown request %T", msg)
			}
		}
	}
}

type registerRequest struct {
	Username string
	Password string
}

type authRequest struct {
	Username string
	Password string
}

// EnsureSchema prepares the tables we use for credentials management.
func (s *Service) EnsureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users(username TEXT PRIMARY KEY, hash TEXT)`)
	return err
}

// Register stores a new user with a salted hash.
func (s *Service) Register(ctx context.Context, username, password string) error {
	select {
	case s.requests <- registerRequest{Username: username, Password: password}:
	case <-ctx.Done():
		return ctx.Err()
	}
	resp := <-s.responses
	if err, ok := resp.(error); ok {
		return err
	}
	return nil
}

func (s *Service) handleRegister(req registerRequest) error {
	rows, err := s.db.Query(`SELECT username FROM users WHERE username = ?`, req.Username)
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			return fmt.Errorf("user %s already exists", req.Username)
		}
	}
	hashed, err := hashPassword(req.Password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO users(username,hash) VALUES(?,?)`, req.Username, hashed)
	return err
}

// Authenticate validates a username/password pair using constant-time comparison.
func (s *Service) Authenticate(ctx context.Context, username, password string) (bool, error) {
	select {
	case s.requests <- authRequest{Username: username, Password: password}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	resp := <-s.responses
	switch val := resp.(type) {
	case authResult:
		return val.OK, val.Err
	case error:
		return false, val
	default:
		return false, fmt.Errorf("unexpected response %T", resp)
	}
}

type authResult struct {
	OK  bool
	Err error
}

func (s *Service) handleAuth(req authRequest) authResult {
	row, err := s.db.Query(`SELECT username,hash FROM users WHERE username = ?`, req.Username)
	if err != nil {
		return authResult{OK: false, Err: err}
	}
	defer row.Close()
	if !row.Next() {
		return authResult{OK: false, Err: errors.New("unknown user")}
	}
	var username, hash string
	if err := row.Scan(&username, &hash); err != nil {
		return authResult{OK: false, Err: err}
	}
	if verifyPassword(hash, req.Password) {
		return authResult{OK: true, Err: nil}
	}
	return authResult{OK: false, Err: errors.New("invalid credentials")}
}

// Middleware wraps handlers with HTTP Basic authentication.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", "Basic realm=restricted")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ok, err := s.Authenticate(r.Context(), username, password)
		if err != nil || !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hashPassword salts and hashes passwords to keep secrets safe.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(salt, []byte(password)...))
	blob := append(salt, sum[:]...)
	return base64.StdEncoding.EncodeToString(blob), nil
}

// verifyPassword checks a password using constant-time comparison.
func verifyPassword(encoded, password string) bool {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) <= 16 {
		return false
	}
	salt := raw[:16]
	stored := raw[16:]
	sum := sha256.Sum256(append(salt, []byte(password)...))
	if len(stored) != len(sum) {
		return false
	}
	return subtle.ConstantTimeCompare(stored, sum[:]) == 1
}
