// Package auth handles JWT signing and verification.
//
// A JWT ("JSON Web Token") is three base64-url-encoded parts separated
// by dots:
//
//	header.payload.signature
//
// Only the signature is secret-derived; the header and payload are just
// base64-encoded JSON — trivially decodable by anyone who has the token.
//
// SIGNED IS NOT ENCRYPTED. Anyone can read the payload; they just can't
// modify it without invalidating the signature. So: never put anything
// truly secret (password, credit-card, etc.) inside a JWT. It's for
// carrying already-non-sensitive identity claims like "user_id = 42."
package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenTTL is how long an issued token is valid.
// 15 minutes is a common short-lived choice:
//   - Short enough that a stolen token can't be used indefinitely.
//   - Long enough that a normal user session doesn't need to re-login
//     every few requests.
//
// Real systems pair this with a longer-lived refresh token so that the
// user's actual login session lasts hours/days without the API needing
// to trust a long-lived JWT. Refresh tokens are a Phase 5 GAP — see
// README.
const TokenTTL = 15 * time.Minute

// Claims is what we put inside the JWT payload.
// jwt.RegisteredClaims embedding adds the standard fields (`exp`, `iat`,
// `iss`, etc.) as JSON keys. Our custom fields are `uid` and (Phase 11)
// `role`.
//
// PHASE 11 NOTE — role in the token:
// The role is baked in at LOGIN time. That means a promotion or
// demotion doesn't take effect until the user's next login (or their
// current token expires — see TokenTTL). Trade for instant revocation
// would be a Redis-backed token denylist or per-request DB lookup;
// both are Phase 5 documented gaps (see README). At 15-min TTL and a
// stub /login endpoint, this is fine for now.
type Claims struct {
	UserID int    `json:"uid"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// Sign creates a signed JWT for the given user + role.
// Signing method HS256 = HMAC + SHA256 with a shared secret. Fine for a
// single-service backend; asymmetric signing (RS256) is only needed when
// you have multiple services verifying tokens issued elsewhere and don't
// want to distribute the signing secret.
//
// The role parameter is passed through to the JWT claim as-is. The
// caller (auth_handler.login) is responsible for reading it from the
// DB so a mint request can't just claim any role — passing a
// user-supplied role directly here would let anyone with /login access
// mint an admin token.
func Sign(userID int, role string, secret []byte) (string, time.Time, error) {
	now := time.Now()
	expires := now.Add(TokenTTL)
	claims := Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expires),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "subscription-service",
			Subject:   strconv.Itoa(userID),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return s, expires, nil
}

// Parse verifies the signature + expiration and returns the user ID
// and role. A single error is returned for any failure mode (bad
// signature, expired, malformed, wrong algorithm) — callers translate
// this to a 401 without leaking which specific check failed, so an
// attacker can't tell "this token is expired" apart from "this token
// was never valid."
var ErrInvalidToken = errors.New("invalid token")

func Parse(tokenString string, secret []byte) (userID int, role string, err error) {
	parsed, perr := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		// Defense in depth: reject tokens whose header claims a different
		// algorithm than what we sign with. Prevents the classic "alg=none"
		// and algorithm-confusion attacks.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return secret, nil
	})
	if perr != nil || !parsed.Valid {
		return 0, "", ErrInvalidToken
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || claims.UserID <= 0 {
		return 0, "", ErrInvalidToken
	}
	// Role can legitimately be empty on tokens minted before Phase 11
	// (pre-role tokens are still valid until TTL). RequireAdmin treats
	// empty role as non-admin, which is the right default.
	return claims.UserID, claims.Role, nil
}
