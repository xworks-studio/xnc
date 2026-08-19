package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"xnc/server/internal/auth"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

// EnsureAdmin 在零用户时创建管理员、default Cluster 与 owner 成员关系；幂等。
func EnsureAdmin(ctx context.Context, st *db.Store, cfg config.Config) error {
	n, err := st.Q().CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if cfg.AdminEmail == "" || cfg.AdminPassword == "" {
		return fmt.Errorf("XNC_ADMIN_EMAIL/XNC_ADMIN_PASSWORD required for first boot")
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		return err
	}
	u, err := st.Q().CreateUser(ctx, sqlc.CreateUserParams{
		ID: uuid.New(), Email: cfg.AdminEmail, DisplayName: "Admin", PasswordHash: hash,
	})
	if err != nil {
		return err
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO clusters(id, name, owner_id) VALUES($1,'default',$2)`,
		uuid.New(), u.ID); err != nil {
		return err
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO cluster_members(cluster_id, user_id, role)
		 SELECT id, $1, 'owner' FROM clusters WHERE name='default'`, u.ID); err != nil {
		return err
	}
	slog.Info("bootstrap admin created", "email", cfg.AdminEmail)
	return nil
}
