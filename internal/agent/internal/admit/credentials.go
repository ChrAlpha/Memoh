package admit

import (
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/felinics/memoh/internal/auth"
)

// RouteCredentialTTL is the lifetime of the credentials a discuss admission or
// a dequeued turn signs. It is the inbound path's, so such a run holds the same
// credentials however it was started.
const RouteCredentialTTL = 5 * time.Minute

// renewedCredentialTTL is how long a credential signed again for a resumed run
// stays valid.
const renewedCredentialTTL = 24 * time.Hour

// Signer signs and reads the credentials of runs the server starts itself. It
// is the only holder of the signing key: a turn started by the server names an
// identity only through a credential signed here.
type Signer struct{ key []byte }

// NewSigner returns a Signer for the signing key.
func NewSigner(key string) *Signer { return &Signer{key: []byte(key)} }

// IssueRoute signs the chat token and the session bearer of a turn on a route.
func (s *Signer) IssueRoute(c auth.RouteCredentials) (chatToken, sessionBearer string, err error) {
	return auth.IssueRouteCredentials(c, string(s.key), RouteCredentialTTL)
}

// Claims reads the allowlisted scope of a credential after authenticating it.
// Expiration and signatures are renewed after restart, never copied to storage.
func (s *Signer) Claims(bearer string) (map[string]string, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if raw == "" {
		return nil, nil
	}
	token, err := jwt.Parse(raw, func(_ *jwt.Token) (any, error) { return s.key, nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return nil, errors.New("invalid session credential for resume")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid session credential claims")
	}
	out := map[string]string{}
	for _, key := range []string{"sub", "user_id", "typ", "bot_id", "chat_id", "route_id", "channel_identity_id"} {
		if value, ok := claims[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	return out, nil
}

// Renew signs the scope claims again with a fresh validity and returns the
// bearer.
func (s *Signer) Renew(claims map[string]string) (string, error) {
	renewed := jwt.MapClaims{}
	for key, value := range claims {
		renewed[key] = value
	}
	now := time.Now()
	renewed["iat"] = now.Unix()
	renewed["exp"] = now.Add(renewedCredentialTTL).Unix()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, renewed).SignedString(s.key)
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}
