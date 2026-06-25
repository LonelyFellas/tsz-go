package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darwish/tsz-go/internal/auth"
)

var ErrInvalidCredentials = errors.New("invalid email or password")

// Service holds the user business logic. It depends on the Repository and the
// auth.TokenManager — both concrete here for simplicity, but easy to turn into
// interfaces for unit testing when needed.
type Service struct {
	repo  *Repository
	token *auth.TokenManager
}

func NewService(repo *Repository, token *auth.TokenManager) *Service {
	return &Service{repo: repo, token: token}
}

func (s *Service) Register(ctx context.Context, email, password, displayName string) (*User, string, error) {
	email = normalizeEmail(email)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("hash password: %w", err)
	}

	u := &User{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  strings.TrimSpace(displayName),
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, "", err // includes ErrEmailTaken
	}

	tok, err := s.token.Generate(u.ID)
	if err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}
	return u, tok, nil
}

func (s *Service) Login(ctx context.Context, email, password string) (*User, string, error) {
	u, err := s.repo.GetByEmail(ctx, normalizeEmail(email))
	if errors.Is(err, ErrNotFound) {
		return nil, "", ErrInvalidCredentials
	}
	if err != nil {
		return nil, "", err
	}

	// Constant-time comparison; same generic error whether the email or the
	// password was wrong, to avoid leaking which accounts exist.
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", ErrInvalidCredentials
	}

	tok, err := s.token.Generate(u.ID)
	if err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}
	return u, tok, nil
}

func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.GetByID(ctx, id)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
