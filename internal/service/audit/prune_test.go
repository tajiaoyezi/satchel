package audit

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
)

// master-scheduler「审计记录按保留期删」：2500 条 200 天前的与 10 条今天的，删掉 2500 条、只剩今天的。
func TestPrune(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		s := service(t, bdb)
		insert := func(at time.Time, n int) {
			rows := make([]model.AuditLog, n)
			for i := range rows {
				rows[i] = model.AuditLog{At: at.UTC(), Actor: "root", ActorKind: "local", Command: "whoami", ArgsDigest: "{}", Result: "ok"}
			}
			if _, err := bdb.NewInsert().Model(&rows).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		insert(time.Now().Add(-200*24*time.Hour), 2500)
		insert(time.Now(), 10)
		n, err := s.Prune(ctx)
		if err != nil || n != 2500 {
			t.Fatalf("应当删掉 2500 条，得到 %d %v", n, err)
		}
		left, err := bdb.NewSelect().Model((*model.AuditLog)(nil)).Count(ctx)
		if err != nil || left != 10 {
			t.Fatalf("应当只剩 10 条，得到 %d %v", left, err)
		}
	})
}
