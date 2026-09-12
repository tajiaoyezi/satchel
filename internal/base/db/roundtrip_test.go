package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
)

func newJob(id string, at time.Time) *model.Job {
	return &model.Job{JobID: id, Kind: "exec", Args: json.RawMessage(`{}`), Status: "queued",
		CreatedAt: at, UpdatedAt: at, ResourceVersion: 1}
}

func TestTimeRoundTripsInUTCAtMicrosecond(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		want := time.Date(2026, 9, 11, 1, 2, 3, 123456789, time.UTC)
		job := newJob("j1", want)
		job.StartedAt = &want
		if _, err := bdb.NewInsert().Model(job).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		var got model.Job
		if err := bdb.NewSelect().Model(&got).Where("id = ?", job.ID).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]time.Time{"created_at": got.CreatedAt, "started_at": *got.StartedAt} {
			if d := v.Sub(want); d < -time.Microsecond || d > time.Microsecond {
				t.Errorf("%s 读回 %v，与写入的 %v 差了 %v，超过微秒", name, v, want, d)
			}
			if _, offset := v.Zone(); offset != 0 {
				t.Errorf("%s 读回的时区偏移应当是 0（UTC），得到 %v", name, v)
			}
		}
	})
}

func TestNullableIntKeepsNullAndZeroApart(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		now := time.Now().UTC()
		zero := int64(0)
		withNull, withZero := newJob("null", now), newJob("zero", now)
		withZero.ExitCode = &zero
		for _, j := range []*model.Job{withNull, withZero} {
			if _, err := bdb.NewInsert().Model(j).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var jobs []model.Job
		if err := bdb.NewSelect().Model(&jobs).Order("id").Scan(ctx); err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 2 {
			t.Fatalf("应当读回两行，得到 %d", len(jobs))
		}
		if jobs[0].ExitCode != nil {
			t.Errorf("写 NULL 的读回应当为空，得到 %d", *jobs[0].ExitCode)
		}
		if jobs[1].ExitCode == nil || *jobs[1].ExitCode != 0 {
			t.Errorf("写 0 的读回应当是 0，得到 %v", jobs[1].ExitCode)
		}
	})
}
