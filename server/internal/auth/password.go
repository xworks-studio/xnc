package auth

import "golang.org/x/crypto/bcrypt"

func HashPassword(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(b), err
}

func VerifyPassword(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}
