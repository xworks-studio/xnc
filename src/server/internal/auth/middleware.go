package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

type ctxKey int

const CtxUser ctxKey = 1

func Middleware(secret []byte, st *db.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				respondAuthErr(w)
				return
			}
			uid, err := ParseToken(secret, strings.TrimPrefix(h, "Bearer "))
			if err != nil {
				respondAuthErr(w)
				return
			}
			id, err := uuid.Parse(uid)
			if err != nil {
				respondAuthErr(w)
				return
			}
			u, err := st.Q().GetUserByID(r.Context(), id)
			if err != nil {
				respondAuthErr(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), CtxUser, u)))
		})
	}
}

func UserFrom(ctx context.Context) sqlc.User {
	return ctx.Value(CtxUser).(sqlc.User)
}

func respondAuthErr(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"invalid token"}}`))
}
