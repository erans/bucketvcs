package web

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type sessionCtxKey struct{}

func withSession(ctx context.Context, s *auth.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// SessionFromContext returns the live session, or nil when the request is anonymous.
func SessionFromContext(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionCtxKey{}).(*auth.Session)
	return s
}

// actorFromSession adapts a session into an *auth.Actor for permission queries.
func actorFromSession(s *auth.Session) *auth.Actor {
	if s == nil {
		return nil
	}
	return &auth.Actor{UserID: s.UserID, Name: s.Name, IsAdmin: s.IsAdmin, Scopes: auth.ScopeLegacy}
}
