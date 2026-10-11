package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/luquinha/vigia/internal/store"
)

const CookieName = "vigia_session"

var (
	ErrInvalidCredentials = errors.New("e-mail ou senha incorretos")
	ErrEmailTaken         = errors.New("já existe uma conta com esse e-mail")
	ErrSignupClosed       = errors.New("cadastro desativado neste servidor")
)

type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

type Service struct {
	db          *store.DB
	ttl         time.Duration
	secure      bool
	allowSignup bool
	dummyHash   []byte
	now         func() time.Time
}

func NewService(db *store.DB, ttl time.Duration, secureCookie, allowSignup bool) *Service {
	dummy, _ := bcrypt.GenerateFromPassword([]byte("senha-que-ninguem-usa"), bcrypt.DefaultCost)
	return &Service{db: db, ttl: ttl, secure: secureCookie, allowSignup: allowSignup, dummyHash: dummy, now: time.Now}
}

func (s *Service) SignupAllowed() bool { return s.allowSignup }

func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", ValidationError{"e-mail inválido"}
	}
	return email, nil
}

func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < 8:
		return ValidationError{"a senha precisa de pelo menos 8 caracteres"}
	case len(pw) > 72:
		return ValidationError{"a senha pode ter no máximo 72 bytes"}
	}
	return nil
}

func (s *Service) Signup(ctx context.Context, email, password string) (store.User, error) {
	if !s.allowSignup {
		return store.User{}, ErrSignupClosed
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return store.User{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return store.User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return store.User{}, fmt.Errorf("hash da senha: %w", err)
	}

	var user store.User
	err = s.db.InTx(ctx, func(q *store.Queries) error {
		user, err = q.CreateUser(ctx, store.CreateUserParams{Email: email, PasswordHash: string(hash), Plan: string(PlanFree)})
		if err != nil {
			if store.IsUniqueViolation(err) {
				return ErrEmailTaken
			}
			return fmt.Errorf("criar usuário: %w", err)
		}
		_, err = CreateProjectWithUniqueSlug(ctx, q, user.ID, "Meu projeto", strings.Split(email, "@")[0], nil, true)
		return err
	})
	if err != nil {
		return store.User{}, err
	}
	return user, nil
}

func (s *Service) Authenticate(ctx context.Context, email, password string) (store.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	user, err := s.db.GetUserByEmail(ctx, email)
	if err != nil {
		if store.IsNotFound(err) {
			bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
			return store.User{}, ErrInvalidCredentials
		}
		return store.User{}, fmt.Errorf("buscar usuário: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return store.User{}, ErrInvalidCredentials
	}
	return user, nil
}

func (s *Service) StartSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	expires := s.now().Add(s.ttl).UTC()
	if err := s.db.CreateSession(ctx, store.CreateSessionParams{TokenHash: hash, UserID: userID, ExpiresAt: expires}); err != nil {
		return fmt.Errorf("criar sessão: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Service) EndSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(CookieName); err == nil {
		if err := s.db.DeleteSession(ctx, hashToken(c.Value)); err != nil {
			return fmt.Errorf("apagar sessão: %w", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Service) userFromRequest(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return store.User{}, false
	}
	user, err := s.db.GetSessionUser(r.Context(), hashToken(c.Value))
	if err != nil {
		return store.User{}, false
	}
	return user, true
}

type ctxKey struct{}

func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

func (s *Service) LoadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.userFromRequest(r); ok {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFrom(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"não autenticado"}`))
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("gerar token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(
		"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
		"é", "e", "ê", "e", "è", "e",
		"í", "i", "î", "i",
		"ó", "o", "ô", "o", "õ", "o", "ö", "o",
		"ú", "u", "ü", "u", "ç", "c", "ñ", "n",
	).Replace(s)
	s = strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func CreateProjectWithUniqueSlug(ctx context.Context, q *store.Queries, userID int64, name, slugBase string, domain *string, public bool) (store.Project, error) {
	base := Slugify(slugBase)
	if base == "" {
		base = "projeto"
	}
	slug := base
	for attempt := range 5 {
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%s", base, strings.ToLower(RandomToken(3)))
			slug = Slugify(slug)
		}
		p, err := q.CreateProject(ctx, store.CreateProjectParams{
			UserID:       userID,
			Name:         name,
			Slug:         slug,
			CustomDomain: domain,
			Public:       public,
		})
		if err == nil {
			return p, nil
		}
		if !store.IsNotFound(err) {
			return store.Project{}, fmt.Errorf("criar projeto: %w", err)
		}
	}
	return store.Project{}, errors.New("não consegui gerar um slug único, tente outro nome")
}
