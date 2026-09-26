package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/schema"
)

// Checkpoint 把 SQLite 的 WAL 写回主库（master-scheduler 的 db_checkpoint，照 mmwx）：先试 TRUNCATE；有读者占着、做不了时
// 退回 PASSIVE，这时 busy 为真、remaining 是还没写回的帧数。只适用于 SQLite。
func Checkpoint(ctx context.Context, bdb *bun.DB) (busy bool, remaining int, err error) {
	if DialectOf(bdb) != schema.SQLite {
		return false, 0, errors.New("WAL 检查点只适用于 SQLite")
	}
	var blocked, logFrames, done int
	if err := bdb.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&blocked, &logFrames, &done); err != nil {
		return false, 0, err
	}
	if blocked == 0 {
		return false, 0, nil
	}
	if err := bdb.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&blocked, &logFrames, &done); err != nil {
		return true, 0, err
	}
	return true, logFrames - done, nil
}

// QuickCheck 检查库是否健康（master-scheduler 的 db_health，照 mmwx）：SQLite 跑 PRAGMA quick_check，结果不是 ok 就返回错误；
// PostgreSQL 只检查连通性。
func QuickCheck(ctx context.Context, bdb *bun.DB) error {
	if DialectOf(bdb) != schema.SQLite {
		return bdb.PingContext(ctx)
	}
	rows, err := bdb.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		if len(problems) > 5 {
			problems = append(problems[:5], fmt.Sprintf("… 共 %d 条", len(problems)))
		}
		return fmt.Errorf("sqlite quick_check 没通过：%s", strings.Join(problems, "；"))
	}
	return nil
}
