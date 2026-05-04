package auth

import (
	"errors"
	"net/http"

	"github.com/yolocs/ocifactory/pkg/logging"
)

// Realm is advertised in the WWW-Authenticate header on 401
// responses so package managers know which realm the credentials
// apply to. Kept short and stable.
const Realm = "ocifactory"

// Middleware returns an http.Handler middleware that runs a on
// every inbound request, installs the resulting Subject on the
// request context, and rejects unauthenticated requests with a 401
// (or 503 when the issuer is unavailable).
//
// On 401, both Bearer and Basic challenges are advertised in
// WWW-Authenticate so package managers that only speak one of the
// two know what to send.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	if a == nil {
		// Wired with no authenticator means "deny everything",
		// not "allow everything". The serve command must
		// explicitly choose AlwaysAnonymous to opt out of authn.
		a = AuthenticatorFunc(func(*http.Request) (*Subject, error) {
			return nil, ErrNoCredential
		})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject, err := a.Authenticate(r)
			if err != nil {
				writeAuthError(w, r, err)
				return
			}
			if subject == nil {
				// Defensive: an authenticator returning
				// (nil, nil) is a contract violation but
				// must not panic the server.
				writeAuthError(w, r, ErrNoCredential)
				return
			}
			r = r.WithContext(WithSubject(r.Context(), subject))
			next.ServeHTTP(w, r)
		})
	}
}

func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	logger := logging.FromContext(r.Context())
	switch {
	case errors.Is(err, ErrIssuerUnavailable):
		logger.WarnContext(r.Context(), "auth: issuer unavailable", "error", err)
		http.Error(w, "issuer unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, ErrNoCredential), errors.Is(err, ErrInvalidToken):
		// Advertise both schemes — pip / twine speaks Bearer
		// after pip 24.1 but older clients and mvn rely on
		// Basic. Listing both means a client that ignores the
		// scheme it doesn't understand still picks the right
		// one.
		w.Header().Add("WWW-Authenticate", `Bearer realm="`+Realm+`"`)
		w.Header().Add("WWW-Authenticate", `Basic realm="`+Realm+`"`)
		if errors.Is(err, ErrNoCredential) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
		} else {
			logger.DebugContext(r.Context(), "auth: invalid token", "error", err)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
		}
	default:
		logger.ErrorContext(r.Context(), "auth: unexpected error", "error", err)
		http.Error(w, "authentication error", http.StatusInternalServerError)
	}
}
