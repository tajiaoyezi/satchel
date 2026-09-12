package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SQLite 的扩展错误码：SQLITE_CONSTRAINT (19) 加上子码左移 8 位。
const (
	sqliteConstraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// PostgreSQL 的 SQLSTATE。
const (
	pgUniqueViolation = "23505"
	pgCheckViolation  = "23514"
)

// translate 把驱动错误翻译成四字段错误：唯一键冲突 → name_taken，CHECK 失败 → bad_request，
// 其它 → database。原始错误只在 Unwrap 链里。
func translate(err error) error {
	if err == nil {
		return nil
	}
	var already *v1.Error
	if errors.As(err, &already) {
		return err
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return nameTaken(err)
		case sqliteConstraintCheck:
			return checkFailed(err)
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return nameTaken(err)
		case pgCheckViolation:
			return checkFailed(err)
		}
	}
	return v1.Wrap(v1.CodeDatabase, "数据库操作失败", err)
}

func nameTaken(cause error) error {
	return v1.Wrap(v1.CodeNameTaken, "名字已被占用：同名对象已存在，或还在软删除的保留期内", cause).
		WithNext("换一个名字，或恢复那条已删除的对象")
}

func checkFailed(cause error) error {
	return v1.Wrap(v1.CodeBadRequest, "写入的值不在允许的范围内", cause)
}
