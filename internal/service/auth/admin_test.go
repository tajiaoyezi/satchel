package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestLookupAndResetAdminPasswordDualDB(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		now := time.Now().UTC()
		seed := func(name string, role v1.Role, deletedAt *time.Time, nSessions int) {
			t.Helper()
			hash, err := HashPassword("old-password")
			if err != nil {
				t.Fatal(err)
			}
			u := &model.User{
				Username: name, Role: string(role), IsActive: true, PasswordHash: hash,
				TotpEnabled: true, TotpSecret: "KEEPME", RecoveryCodes: json.RawMessage(`["h1","h2"]`),
				NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`),
				CreatedAt: now, UpdatedAt: now, ResourceVersion: 1, DeletedAt: deletedAt,
			}
			if _, err := bdb.NewInsert().Model(u).Exec(ctx); err != nil {
				t.Fatal(err)
			}
			srepo := sessions.New(bdb)
			for i := 0; i < nSessions; i++ {
				if err := srepo.Insert(ctx, sessions.Session{
					TokenHash: "tok-" + name + "-" + string(rune('a'+i)),
					Username:  name, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
				}); err != nil {
					t.Fatal(err)
				}
			}
		}

		seed("admin", v1.RoleAdmin, nil, 2)
		seed("bob", v1.RoleUser, nil, 2)
		deletedAt := now.Add(-time.Minute)
		seed("ghost", v1.RoleAdmin, &deletedAt, 0)

		got, err := LookupAdmin(ctx, bdb, "admin")
		if err != nil || got.Username != "admin" {
			t.Fatalf("LookupAdmin(admin) = %+v, %v", got, err)
		}
		if _, err := LookupAdmin(ctx, bdb, "bob"); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("LookupAdmin(user) want bad_request, got %v", err)
		}
		if _, err := LookupAdmin(ctx, bdb, "ghost"); err == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("LookupAdmin(deleted) want not_found, got %v", err)
		}
		if _, err := LookupAdmin(ctx, bdb, "nobody"); err == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("LookupAdmin(missing) want not_found, got %v", err)
		}

		revoked, err := ResetAdminPassword(ctx, bdb, "admin", "brand-new-pw")
		if err != nil || revoked != 2 {
			t.Fatalf("ResetAdminPassword = %d, %v; want 2, nil", revoked, err)
		}
		a, err := users.New(bdb, store.New(bdb, schema.Default())).GetByUsername(ctx, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if !CheckPassword(a.PasswordHash, "brand-new-pw") || CheckPassword(a.PasswordHash, "old-password") {
			t.Fatal("new password should match and old password should fail")
		}
		if !a.TOTPEnabled || a.TOTPSecret != "KEEPME" || len(a.RecoveryCodes) != 2 {
			t.Fatalf("2FA state must remain untouched: %+v", a)
		}
		if n, _ := sessions.New(bdb).CountByUser(ctx, "admin", time.Now()); n != 0 {
			t.Fatalf("admin sessions should be 0, got %d", n)
		}
		if n, _ := sessions.New(bdb).CountByUser(ctx, "bob", time.Now()); n != 2 {
			t.Fatalf("bob sessions should be 2, got %d", n)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "bob", "brand-new-pw"); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("reset non-admin want bad_request, got %v", err)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "ghost", "brand-new-pw"); err == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("reset deleted want not_found, got %v", err)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "admin", "short"); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("weak password want bad_request, got %v", err)
		}
	})
}
