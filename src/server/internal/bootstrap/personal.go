package bootstrap

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"xnc/server/internal/db/sqlc"
)

// 个人默认 cluster（0005）：建号事务内为用户自动创建一个 personal=true 的
// cluster + owner 成员，保证"每个用户至少有一个存活 cluster 且角色 owner"
// 的不变式（xnc register 不再 CLUSTER_NOT_FOUND）。命名规则与迁移 0005 的
// 存量回填一致。

var localPartRe = regexp.MustCompile(`[^a-z0-9._-]`)

// PersonalClusterName 由 email 生成个人默认 cluster 基名：本地部分小写、仅留
// [a-z0-9._-]、截 40 字符 + "-default"；清洗后为空（或本地部分为空）则
// "user-default"。
func PersonalClusterName(email string) string {
	local := strings.ToLower(email)
	if at := strings.Index(local, "@"); at >= 0 {
		local = local[:at]
	}
	local = localPartRe.ReplaceAllString(local, "")
	if local == "" {
		return "user-default"
	}
	if len(local) > 40 {
		local = local[:40]
	}
	return local + "-default"
}

// EnsurePersonalCluster 为用户创建个人默认 cluster + owner 成员，绑定调用方
// 事务（与建号同一原子域）。并发同 localpart 建号（john@a.com / john@b.com）
// 在名字上竞态：PG 语句失败会中止整个事务，故每次尝试包 SAVEPOINT——撞
// 唯一索引时回滚到保存点换 -2/-3… 后缀重试（上限 5 次，仍撞属异常，返回
// 错误让建号整体回滚）。
func EnsurePersonalCluster(ctx context.Context, tx pgx.Tx, user sqlc.User) (sqlc.Cluster, error) {
	q := sqlc.New(tx)
	base := PersonalClusterName(user.Email)
	name := base
	for i := 2; ; i++ {
		if _, err := tx.Exec(ctx, "SAVEPOINT personal_cluster"); err != nil {
			return sqlc.Cluster{}, err
		}
		c, err := q.CreateCluster(ctx, sqlc.CreateClusterParams{
			ID: uuid.New(), Name: name, OwnerID: user.ID, Personal: true,
		})
		if err == nil {
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT personal_cluster"); err != nil {
				return sqlc.Cluster{}, err
			}
			if err := q.AddMembership(ctx, sqlc.AddMembershipParams{
				ClusterID: c.ID, UserID: user.ID, Role: "owner",
			}); err != nil {
				return sqlc.Cluster{}, err
			}
			return c, nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || i > 5 {
			return sqlc.Cluster{}, err
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT personal_cluster"); err != nil {
			return sqlc.Cluster{}, err
		}
		// 截回基名再加序号，避免名字随重试无限增长（与迁移 0005 同款）。
		if len(base) > 40 {
			base = base[:40]
		}
		name = base + "-" + strconv.Itoa(i)
	}
}
