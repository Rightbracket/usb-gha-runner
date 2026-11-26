// Package auth stores the admin password (PBKDF2-SHA256) and issues
// in-memory session cookies for ghr-web.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

const (
	iterations = 600_000
	cookieName = "ghr_session"
	sessionTTL = 12 * time.Hour
	MinLength  = 10
)

type record struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	Iter int    `json:"iter"`
}

type Store struct {
	Path string

	setupMu  sync.Mutex
	mu       sync.Mutex
	sessions map[string]time.Time
	fails    map[string][]time.Time
}

func NewStore(path string) *Store {
	return &Store{Path: path, sessions: map[string]time.Time{}, fails: map[string][]time.Time{}}
}

func (s *Store) HasPassword() bool {
	_, err := os.Stat(s.Path)
	return err == nil
}

func (s *Store) SetPassword(pw string) error {
	if len(pw) < MinLength {
		return errors.New("password must be at least 10 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	h, err := pbkdf2.Key(sha256.New, pw, salt, iterations, 32)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(record{Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(h), Iter: iterations})
	if err := config.WriteFileAtomic(s.Path, b, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.sessions = map[string]time.Time{} // log everyone out
	s.mu.Unlock()
	return nil
}

// SetPasswordIfUnset is used for first-visitor setup; it fails if a password
// already exists so two racing visitors can't both claim the box.
func (s *Store) SetPasswordIfUnset(pw string) error {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.HasPassword() {
		return errors.New("a password is already set")
	}
	return s.SetPassword(pw)
}

func (s *Store) Check(pw string) bool {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return false
	}
	var r record
	if json.Unmarshal(b, &r) != nil {
		return false
	}
	salt, _ := hex.DecodeString(r.Salt)
	want, _ := hex.DecodeString(r.Hash)
	got, err := pbkdf2.Key(sha256.New, pw, salt, r.Iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// Allow rate-limits login attempts: 5 failures per client per 5 minutes.
func (s *Store) Allow(client string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-5 * time.Minute)
	var keep []time.Time
	for _, t := range s.fails[client] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	s.fails[client] = keep
	return len(keep) < 5
}

func (s *Store) Fail(client string) {
	s.mu.Lock()
	s.fails[client] = append(s.fails[client], time.Now())
	s.mu.Unlock()
}

func (s *Store) Login(w http.ResponseWriter) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[id] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
}

func (s *Store) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true})
}

func (s *Store) Valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

// ClientKey identifies a client for rate limiting (IP without port).
func ClientKey(r *http.Request) string {
	h := r.RemoteAddr
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return h
}
