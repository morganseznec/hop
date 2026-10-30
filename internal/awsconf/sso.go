package awsconf

import (
	"encoding/json"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
)

type TokenState int

const (
	TokenMissing TokenState = iota
	TokenExpired
	TokenValid
)

type Token struct {
	State     TokenState
	ExpiresAt time.Time
}

func (t Token) String() string {
	switch t.State {
	case TokenValid:
		return "valid until " + t.ExpiresAt.Local().Format("15:04")
	case TokenExpired:
		return "expired " + t.ExpiresAt.Local().Format("Jan 02 15:04")
	}
	return "missing"
}

// Token reads the cached token without any network call. The file is the one shared
// by the AWS CLI and the SDK: ~/.aws/sso/cache/<sha1 of the key>.json.
//
// An expired token that carries a refreshToken could still be renewed by the SDK. It
// is reported as expired anyway: in practice the underlying IAM Identity Center
// session has the same lifetime, so the refresh fails ("refresh failed").
func (p Portal) Token() Token {
	path, err := ssocreds.StandardCachedTokenFilepath(p.Key)
	if err != nil {
		return Token{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Token{}
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if json.Unmarshal(b, &tok) != nil || tok.AccessToken == "" {
		return Token{}
	}
	exp, err := time.Parse(time.RFC3339, tok.ExpiresAt)
	if err != nil {
		return Token{}
	}
	// One minute of margin: a token that expires while the session opens is useless.
	if time.Until(exp) < time.Minute {
		return Token{State: TokenExpired, ExpiresAt: exp}
	}
	return Token{State: TokenValid, ExpiresAt: exp}
}
