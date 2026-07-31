package internal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var JWTSecret []byte
var JWTSecretEphemeral bool

func init() {
	var err error
	JWTSecret, JWTSecretEphemeral, err = ResolveJWTSecret(os.Getenv("JWT_SECRET"))
	if err != nil {
		panic(err)
	}
}

func ResolveJWTSecret(value string) ([]byte, bool, error) {
	if value != "" {
		if len(value) < 32 {
			return nil, false, fmt.Errorf("JWT_SECRET must be at least 32 bytes")
		}
		return []byte(value), false, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, false, fmt.Errorf("generate JWT secret: %w", err)
	}
	return secret, true, nil
}

func GenerateToken(userID int64, username string) (string, error) {
	header := jwtHeaderEncoded
	payload, err := json.Marshal(struct {
		Sub  int64  `json:"sub"`
		Name string `json:"name"`
		Exp  int64  `json:"exp"`
	}{
		Sub:  userID,
		Name: username,
		Exp:  time.Now().Add(72 * time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	payloadEnc := base64url(payload)
	sig := hmacSHA256(header+"."+payloadEnc, JWTSecret)
	return header + "." + payloadEnc + "." + sig, nil
}

func ValidateToken(token string) (int64, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, "", fmt.Errorf("invalid token")
	}
	if parts[0] != jwtHeaderEncoded {
		return 0, "", fmt.Errorf("invalid token header")
	}
	expectedSig := hmacSHA256(parts[0]+"."+parts[1], JWTSecret)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return 0, "", fmt.Errorf("invalid signature")
	}
	payload, err := base64urlDecode(parts[1])
	if err != nil {
		return 0, "", err
	}
	var claims struct {
		Sub  int64  `json:"sub"`
		Name string `json:"name"`
		Exp  int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, "", err
	}
	if time.Now().Unix() > claims.Exp {
		return 0, "", fmt.Errorf("token expired")
	}
	return claims.Sub, claims.Name, nil
}

var jwtHeaderEncoded = base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))

func hmacSHA256(data string, key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return base64url(h.Sum(nil))
}

func base64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func base64urlDecode(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

func GenerateSetupToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func SetupTokenMatches(expected, provided string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}
