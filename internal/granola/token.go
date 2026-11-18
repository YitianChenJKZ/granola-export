package granola

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const tokenFileName = "supabase.json"

// Token is the login the Granola app keeps on disk for its own use.
type Token struct {
	AccessToken string
	// ExpiresAt is zero when the expiry could not be determined.
	ExpiresAt time.Time
	UserID    string
}

// Expired reports whether the token has expired or is about to.
func (t *Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt.Add(-time.Minute))
}

// LoadToken reads the app's login from supabase.json in dir. The error is
// os.ErrNotExist if there is none, ErrEncrypted if Granola encrypts it, and a
// *ShapeError if the file is laid out differently than expected.
func LoadToken(dir string) (*Token, error) {
	path := filepath.Join(dir, tokenFileName)
	if err := plaintextFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top fields
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, &ShapeError{tokenFileName, "not a JSON object"}
	}

	var tokens fields
	for _, key := range []string{"workos_tokens", "cognito_tokens"} {
		if f, ok := nestedObject(top[key]); ok && f.str("access_token") != "" {
			tokens = f
			break
		}
	}
	if tokens == nil {
		return nil, &ShapeError{tokenFileName, "no access token under workos_tokens or cognito_tokens; top-level keys: " + top.keys()}
	}

	t := &Token{AccessToken: tokens.str("access_token")}
	if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
		t.ExpiresAt = exp
	} else {
		var obtainedMillis, expiresInSeconds float64
		_ = json.Unmarshal(tokens["obtained_at"], &obtainedMillis)
		_ = json.Unmarshal(tokens["expires_in"], &expiresInSeconds)
		if obtainedMillis > 0 && expiresInSeconds > 0 {
			t.ExpiresAt = time.UnixMilli(int64(obtainedMillis)).Add(time.Duration(expiresInSeconds) * time.Second)
		}
	}
	if user, ok := nestedObject(top["user_info"]); ok {
		t.UserID = user.str("id")
	}
	return t, nil
}

// jwtExpiry returns the exp claim of a JWT, or zero if token is not one.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0)
}

// DescribeTokenError turns a LoadToken error into advice for the user.
func DescribeTokenError(err error) string {
	switch {
	case os.IsNotExist(err):
		return "no Granola login found (" + tokenFileName + " is missing); is Granola installed and signed in on this Mac?"
	case isEncrypted(err):
		return "Granola stores its login encrypted on this Mac, which this tool does not unlock"
	default:
		return fmt.Sprintf("cannot read the Granola login: %v", err)
	}
}
