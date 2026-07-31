package postgres

// DB-gated regression test for DEFECT-04: scanning a NULL avatar_url into the
// User.AvatarURL field. The unit tests use a fake repo and cannot exercise the
// pgx scan path, so this test guards the real scanUser against NULL avatar_url
// (admin seed user) regressing back to a non-pointer string and HTTP 500.
//
// Skipped unless DATABASE_URL is set, mirroring test/integration:
//   DATABASE_URL=... go test ./internal/infra/postgres/...

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
)

func TestUserRepo_List_NullAvatarURLScan(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repo := NewUserRepo(NewDB(pool))

	// One user with NULL avatar_url (reproduces the admin-seed defect condition)
	// and one with a set avatar_url, to cover both scan branches.
	const nullEmail = "ys27_null@wiki.local"
	const setEmail = "ys27_set@wiki.local"
	const avatar = "https://cdn.local/avatar.png"
	for _, e := range []string{nullEmail, setEmail} {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email=$1`, e)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,'YS27 Null','active')`, nullEmail)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO users (email, name, status, avatar_url) VALUES ($1,'YS27 Set','active',$2)`,
		setEmail, avatar)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email IN ($1,$2)`, nullEmail, setEmail)
	})

	// Admin scope returns all active users; the scan must not error on NULL avatar_url.
	got, _, err := repo.List(ctx, service.UserQuery{
		IsAdmin: true,
		Params:  pagination.Params{Page: 1, PageSize: 1000},
	})
	require.NoError(t, err, "List must not 500 on NULL avatar_url")

	var nullU, setU *domain.User
	for i := range got {
		switch got[i].Email {
		case nullEmail:
			nullU = &got[i]
		case setEmail:
			setU = &got[i]
		}
	}
	require.NotNil(t, nullU, "NULL-avatar user should be returned")
	require.NotNil(t, setU, "set-avatar user should be returned")

	assert.Nil(t, nullU.AvatarURL, "NULL avatar_url must scan to nil, not error")
	require.NotNil(t, setU.AvatarURL, "set avatar_url must scan to a non-nil pointer")
	assert.Equal(t, avatar, *setU.AvatarURL)
}
