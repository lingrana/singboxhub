package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword hashes a password with bcrypt (cost 10).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	return string(hash), err
}

// ComparePassword verifies a password against a bcrypt hash. Timing is
// uniform regardless of input length.
func ComparePassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
