package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func MakeToken(secret []byte, userID string, ttl time.Duration) (string, error) {
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

func ParseToken(secret []byte, tok string) (string, error) {
	c := jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tok, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return secret, nil
	})
	if err != nil {
		return "", err
	}
	if c.Subject == "" {
		return "", errors.New("empty subject")
	}
	return c.Subject, nil
}
