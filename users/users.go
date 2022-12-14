package users

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path"
	"time"

	"github.com/alesr/stdservices/pkg/validate"
	"github.com/alesr/stdservices/users/repository"
	"go.uber.org/zap"

	"github.com/golang-jwt/jwt"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var (
	_                Service                = (*DefaultService)(nil)
	jwtSigningMethod *jwt.SigningMethodHMAC = jwt.SigningMethodHS512
)

type (
	// Service defines the service interface
	Service interface {
		// Create creates a new user and returns the created user with its ID and "user" role
		Create(ctx context.Context, in CreateUserInput) (*User, error)

		// Delete soft deletes a user by id
		Delete(ctx context.Context, id string) error

		// FetchByID fetches a non-deleted user by id and returns the user
		FetchByID(ctx context.Context, id string) (*User, error)

		// GenerateToken generates a JWT token for the user
		GenerateToken(ctx context.Context, email, password string) (string, error)

		// VerifyToken verifies a JWT token and returns the user username, id and role
		VerifyToken(ctx context.Context, token string) (*VerifyTokenResponse, error)

		// SendEmailVerification sends an email verification to the user.
		// The user must be created before calling this method.
		SendEmailVerification(ctx context.Context, userID, username, to string) error
	}

	repo interface {
		Insert(ctx context.Context, user *repository.User) (*repository.User, error)
		SelectByID(ctx context.Context, id string) (*repository.User, error)
		SelectByEmail(ctx context.Context, email string) (*repository.User, error)
		DeleteByID(ctx context.Context, id string) error
		InsertEmailVerification(ctx context.Context, in repository.EmailVerification) error
	}

	emailer interface {
		Send(from, to string, body []byte) error
	}

	jwtClaim struct {
		id   string
		role string
		jwt.StandardClaims
	}
)

type ServiceOption func(*DefaultService)

func WithEmailVerification(fromName, fromAddr, endpoint string, emailer emailer) ServiceOption {
	return func(s *DefaultService) {
		s.emailer = emailer
		s.emailVerificationSenderName = fromName
		s.emailVerificationSenderAddr = fromAddr
		s.emailVerificationEndpoint = endpoint
	}
}

type DefaultService struct {
	logger                      *zap.Logger
	jwtSigningKey               string
	emailVerificationSenderName string
	emailVerificationSenderAddr string
	emailVerificationEndpoint   string
	emailer                     emailer
	repo                        repo
}

// New instantiates a new users service
func New(logger *zap.Logger, jwtSigningKey string, repo repo, opts ...ServiceOption) *DefaultService {
	service := DefaultService{
		logger:        logger,
		jwtSigningKey: jwtSigningKey,
		repo:          repo,
	}

	for _, opt := range opts {
		opt(&service)
	}
	return &service
}

// Create creates a new user and returns the created user
func (s *DefaultService) Create(ctx context.Context, in CreateUserInput) (*User, error) {
	if err := in.validate(); err != nil {
		return nil, fmt.Errorf("could not validate create user input: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("could not hash password: %s", err)
	}

	insertedUser, err := s.repo.Insert(ctx, &repository.User{
		ID:            uuid.NewString(),
		Fullname:      in.Fullname,
		Username:      in.Username,
		Birthdate:     in.Birthdate,
		Email:         in.Email,
		EmailVerified: false,
		PasswordHash:  string(hash),
		Role:          string(RoleUser),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	})
	if err != nil {
		if errors.Is(err, repository.ErrDuplicateRecord) {
			return nil, errAlreadyExists
		}
		return nil, fmt.Errorf("could not insert user: %s", err)
	}

	user, err := newUserFromRepository(insertedUser)
	if err != nil {
		return nil, fmt.Errorf("could not parse storage user to domain model: %s", err)
	}

	if s.emailer != nil {
		if err := s.SendEmailVerification(ctx, user.ID, user.Username, user.Email); err != nil {
			// It doesn't matter if the email verification fails.
			// The next time an API call is made, a new verification will can be requested
			s.logger.Error("could not send email verification", zap.String("user_id", user.ID), zap.Error(err))
		}
	}
	return user, nil
}

// FetchByID fetches a user by id and returns the user
func (s *DefaultService) FetchByID(ctx context.Context, id string) (*User, error) {
	if err := validate.ID(id); err != nil {
		return nil, fmt.Errorf("could not validate id: %w", err)
	}

	storageUser, err := s.repo.SelectByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("could not select user by id: %s", err)
	}

	if storageUser == nil {
		return nil, errNotFound
	}

	user, err := newUserFromRepository(storageUser)
	if err != nil {
		return nil, fmt.Errorf("could not parse storage user to domain model: %s", err)
	}
	return user, nil
}

func (s *DefaultService) Delete(ctx context.Context, id string) error {
	if err := validate.ID(id); err != nil {
		return fmt.Errorf("could not validate id: %w", err)
	}

	if err := s.repo.DeleteByID(ctx, id); err != nil {
		return fmt.Errorf("could not delete user by id: %s", err)
	}
	return nil
}

// GenerateToken generates a JWT token for the user
func (s *DefaultService) GenerateToken(ctx context.Context, email, password string) (string, error) {
	if err := validate.Email(email); err != nil {
		return "", fmt.Errorf("could not validate email: %s", err)
	}

	if err := validate.Password(password); err != nil {
		return "", fmt.Errorf("could not validate password: %s", err)
	}

	// Fetch user by username
	storageUser, err := s.repo.SelectByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("could not select user by email: %s", err)
	}

	// Check if user exists
	if storageUser == nil {
		return "", errNotFound
	}

	// Check if password is correct
	if err := bcrypt.CompareHashAndPassword([]byte(storageUser.PasswordHash), []byte(password)); err != nil {
		return "", errPasswordInvalid
	}

	// Generate JWT
	token, err := s.generateJWT(storageUser.ID, role(storageUser.Role))
	if err != nil {
		return "", fmt.Errorf("could not generate jwt: %s", err)
	}
	return token, nil
}

// VerifyToken verifies a JWT token and returns the authentication data
func (s *DefaultService) VerifyToken(ctx context.Context, token string) (*VerifyTokenResponse, error) {
	if token == "" {
		return nil, errTokenEmpty
	}

	jwtToken, err := jwt.Parse(token, func(token *jwt.Token) (interface{}, error) {
		method, ok := token.Method.(*jwt.SigningMethodHMAC)
		if !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		if method.Alg() != jwtSigningMethod.Alg() {
			return nil, errors.New("invalid token signing method")
		}
		return []byte(s.jwtSigningKey), nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not parse token: %s", err)
	}

	claims, ok := jwtToken.Claims.(jwt.MapClaims)
	if !ok || !jwtToken.Valid {
		return nil, errTokenInvalid
	}

	userID, ok := claims["user_id"].(string)
	if !ok {
		return nil, fmt.Errorf("could not find user id in token")
	}

	role, ok := claims["role"].(string)
	if !ok {
		return nil, fmt.Errorf("could not find role in token")
	}

	expiration, ok := claims["exp"].(float64)
	if !ok {
		return nil, fmt.Errorf("could not find expiration in token")
	}

	if time.Unix(int64(expiration), 0).Before(time.Now()) {
		return nil, errTokenExpired
	}

	storageUser, err := s.repo.SelectByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("could not select user by id: %s", err)
	}

	if storageUser == nil {
		return nil, errNotFound
	}

	return &VerifyTokenResponse{
		ID:       storageUser.ID,
		Username: storageUser.Username,
		Role:     role,
	}, nil
}

func (s *DefaultService) SendEmailVerification(ctx context.Context, userID, username, to string) error {
	code := randString(6)

	in := repository.EmailVerification{
		Code:      code,
		UserID:    userID,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour * 24),
	}

	if err := s.repo.InsertEmailVerification(ctx, in); err != nil {
		return fmt.Errorf("could not insert email verification: %s", err)
	}

	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s Email Verification\r\n\r\nPlease click the following link to verify your email address: %s\r\n",
		s.emailVerificationSenderAddr, to, s.emailVerificationSenderName, path.Join(s.emailVerificationEndpoint, code))

	if err := s.emailer.Send(s.emailVerificationSenderName, to, []byte(body)); err != nil {
		return fmt.Errorf("could not send email verification: %s", err)
	}
	return nil
}

func (s *DefaultService) generateJWT(userID string, role role) (string, error) {
	if err := validate.ID(userID); err != nil {
		return "", fmt.Errorf("could not validate id: %w", err)
	}

	if err := role.validate(); err != nil {
		return "", errRoleInvalid
	}

	now := time.Now().UTC()

	token := jwt.NewWithClaims(jwtSigningMethod, jwtClaim{
		userID,
		string(role),
		jwt.StandardClaims{
			IssuedAt:  now.Unix(),
			ExpiresAt: now.Add(time.Hour * 24).Unix(),
		},
	})

	signedString, err := token.SignedString([]byte(s.jwtSigningKey))
	if err != nil {
		return "", fmt.Errorf("could not sign token: %s", err)
	}

	return signedString, nil
}

func newUserFromRepository(user *repository.User) (*User, error) {
	var role role
	switch user.Role {
	case "user":
		role = RoleUser
	case "admin":
		role = RoleAdmin
	default:
		return nil, fmt.Errorf("invalid role: %s", user.Role)
	}

	return &User{
		ID:            user.ID,
		Fullname:      user.Fullname,
		Username:      user.Username,
		Birthdate:     user.Birthdate,
		Email:         user.Email,
		EmailVerified: user.EmailVerified,
		Role:          role,
		CreatedAt:     user.CreatedAt,
		UpdatedAt:     user.UpdatedAt,
	}, nil
}

const chars = "abcdefghijklmnopqrstuvwxyz0123456789"

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

func randString(length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = chars[seededRand.Intn(len(chars))]
	}
	return string(b)
}
