package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-kid-accounts Phase 2 — the storage half of the kid role: the allow-list
// (kid_allowed_services), VisibleServiceIDs as the single filter choke point,
// kid creation defaults, and the cascades of AC-014. Like the rest of this
// package these run against the shared test DB without truncating it, so every
// row they create is uniquely named.

var kidSeq int

func uniqueKidName(t *testing.T) string {
	t.Helper()
	kidSeq++
	// Kid names are capped at 40 characters by the API; storage does not
	// care, but keep fixtures realistic and unique.
	return fmt.Sprintf("kid-%d-%d", kidSeq, len(t.Name()))
}

func seedStoreAdmin(t *testing.T, store *storage.Store, ctx context.Context) string {
	t.Helper()
	catSeq++
	u, err := store.CreateUser(ctx, fmt.Sprintf("stg-admin-%s-%d@example.com", t.Name(), catSeq), "x", "admin")
	require.NoError(t, err)
	return u.ID
}

func seedStoreService(t *testing.T, store *storage.Store, ctx context.Context, ownerID string) string {
	t.Helper()
	catSeq++
	sv, err := store.CreateService(ctx, ownerID, storage.Service{
		Slug: fmt.Sprintf("svc-%d", catSeq), Name: fmt.Sprintf("Svc %d", catSeq), URL: "https://example.com",
	})
	require.NoError(t, err)
	return sv.ID
}

func rawConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, requireDatabaseURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

// TASK-202 — nil means "everything" for the two adult roles.
func TestVisibleServiceIDs_AdultsSeeEverything(t *testing.T) {
	store, ctx := openStore(t)
	for _, role := range []string{"admin", "user"} {
		vis, err := store.VisibleServiceIDs(ctx, storage.User{ID: "00000000-0000-0000-0000-000000000000", Role: role})
		require.NoError(t, err)
		assert.Nil(t, vis, "%s: VisibleServiceIDs must be nil (everything)", role)
		assert.True(t, vis.Allows("any-id"), "%s: a nil set allows every id", role)
	}
}

// TASK-202 — a kid sees exactly the allow-list. An EMPTY allow-list must come
// back as an empty, non-nil set: nil would mean "everything", which is the one
// mistake that would show a kid with no apps the entire catalog.
func TestVisibleServiceIDs_KidSeesOnlyAllowList(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	a := seedStoreService(t, store, ctx, admin)
	b := seedStoreService(t, store, ctx, admin)

	kid, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)

	vis, err := store.VisibleServiceIDs(ctx, kid)
	require.NoError(t, err)
	require.NotNil(t, vis, "a kid with no apps gets an empty set, never nil")
	assert.Equal(t, 0, vis.Len())
	assert.False(t, vis.Allows(a))
	assert.False(t, vis.Allows(b))

	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{a}))
	vis, err = store.VisibleServiceIDs(ctx, kid)
	require.NoError(t, err)
	require.NotNil(t, vis)
	assert.Equal(t, 1, vis.Len())
	assert.True(t, vis.Allows(a))
	assert.False(t, vis.Allows(b))
	assert.False(t, vis.Allows(""), "the empty id is never allowed")
}

// An unknown role is treated like a kid (allow-list, here empty), never like
// an adult: the filter fails closed.
func TestVisibleServiceIDs_UnknownRoleFailsClosed(t *testing.T) {
	store, ctx := openStore(t)
	for _, role := range []string{"", "Admin", "superuser"} {
		vis, err := store.VisibleServiceIDs(ctx, storage.User{ID: "00000000-0000-0000-0000-000000000000", Role: role})
		require.NoError(t, err)
		require.NotNil(t, vis, "role %q must not see everything", role)
		assert.Equal(t, 0, vis.Len())
	}
}

// AC-001 / AC-019 / UX K-3 — a kid is created with only a name and color, NULL
// email and password, the health bar off and the large density.
func TestCreateKid_DefaultsAndNullCredentials(t *testing.T) {
	store, ctx := openStore(t)
	name := uniqueKidName(t)
	kid, err := store.CreateKid(ctx, name, "teal")
	require.NoError(t, err)

	assert.Equal(t, "kid", kid.Role)
	assert.Equal(t, name, kid.DisplayName)
	assert.Equal(t, "teal", kid.AvatarColor)
	assert.False(t, kid.ShowHealthBar, "AC-019: the kid health bar is off by default")
	assert.Equal(t, "large", kid.DensityPref, "UX K-3: kids default to the large density")
	assert.Equal(t, "", kid.Email)
	assert.Equal(t, "", kid.PasswordHash)

	conn := rawConn(t, ctx)
	var emailNull, pwNull bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT email IS NULL, password_hash IS NULL FROM users WHERE id = $1`, kid.ID).Scan(&emailNull, &pwNull))
	assert.True(t, emailNull, "a kid's email is NULL in the database")
	assert.True(t, pwNull, "a kid's password_hash is NULL in the database")

	// Reading a kid back must not trip over the NULL email/password.
	got, err := store.UserByID(ctx, kid.ID)
	require.NoError(t, err)
	assert.Equal(t, kid, got)
}

// The CHECK constraint: NULL credentials only for kids, and kids only with NULL
// credentials. Adults keep the old NOT NULL guarantee. Each rejected insert
// must fail on the named CHECK, not on something incidental, or the test
// would pass vacuously.
func TestKidCredentialsCheckConstraint(t *testing.T) {
	store, ctx := openStore(t)
	conn := rawConn(t, ctx)
	insert := func(email, pw *string, role string, name *string) error {
		_, err := conn.Exec(ctx,
			`INSERT INTO users (email, password_hash, role, display_name, show_uptime_display) VALUES ($1, $2, $3, $4, TRUE)`,
			email, pw, role, name)
		return err
	}
	str := func(s string) *string { return &s }
	uniqueEmail := func() *string { catSeq++; return str(fmt.Sprintf("chk-%d@example.com", catSeq)) }

	assertCheck(t, insert(uniqueEmail(), nil, "kid", str(uniqueKidName(t))), "users_kid_credentials_check", "a kid with an email")
	assertCheck(t, insert(nil, str("x"), "kid", str(uniqueKidName(t))), "users_kid_credentials_check", "a kid with a password hash")
	assertCheck(t, insert(nil, nil, "kid", nil), "users_kid_credentials_check", "a kid without a display name")
	for _, role := range []string{"admin", "user"} {
		assertCheck(t, insert(nil, str("x"), role, nil), "users_kid_credentials_check", role+" with a NULL email")
		assertCheck(t, insert(uniqueEmail(), nil, role, nil), "users_kid_credentials_check", role+" with a NULL password")
	}
	assertCheck(t, insert(uniqueEmail(), str("x"), "owner", nil), "users_role_check", "an unknown role")

	// The positive controls: the same insert shape succeeds for a valid kid
	// and a valid adult, so the failures above are the CHECKs and nothing else.
	require.NoError(t, insert(nil, nil, "kid", str(uniqueKidName(t))))
	require.NoError(t, insert(uniqueEmail(), str("x"), "user", nil))

	// CreateUser (the register / OIDC path) can never mint a kid: it always
	// carries an email, which the CHECK refuses for a kid.
	_, err := store.CreateUser(ctx, *uniqueEmail(), "x", "kid")
	assertCheck(t, err, "users_kid_credentials_check", "CreateUser with role kid")
}

func assertCheck(t *testing.T, err error, constraint, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if assert.True(t, errors.As(err, &pgErr), "%s: want a CHECK violation, got %v", what, err) {
		assert.Equal(t, "23514", pgErr.Code, "%s: want a CHECK violation, got %v", what, err)
		assert.Equal(t, constraint, pgErr.ConstraintName, "%s", what)
	}
}

// UX §3 — names are unique among kids, case-insensitively.
func TestCreateKid_NameUniqueAmongKids(t *testing.T) {
	store, ctx := openStore(t)
	name := uniqueKidName(t)
	_, err := store.CreateKid(ctx, name, "blue")
	require.NoError(t, err)

	_, err = store.CreateKid(ctx, name, "red")
	assert.ErrorIs(t, err, storage.ErrKidNameTaken)
	_, err = store.CreateKid(ctx, "KID"+name[3:], "red")
	assert.ErrorIs(t, err, storage.ErrKidNameTaken, "uniqueness is case-insensitive")
}

// UserByEmail cannot resolve to a kid: kids have no email, so the login and
// OIDC paths (both keyed by email) can never reach one.
func TestUserByEmail_NeverReturnsKid(t *testing.T) {
	store, ctx := openStore(t)
	_, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	_, err = store.UserByEmail(ctx, "")
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// The first-account-becomes-admin rule counts adults only, so kids can never
// change who bootstraps as admin.
func TestCountUsers_IgnoresKids(t *testing.T) {
	store, ctx := openStore(t)
	before, err := store.CountUsers(ctx)
	require.NoError(t, err)
	_, err = store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	after, err := store.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after, "creating a kid must not change CountUsers")
}

// SetKidServices is a whole-set replace; ids outside the shared catalog are
// refused (and nothing changes); duplicates collapse.
func TestSetKidServices_ReplaceAndValidate(t *testing.T) {
	store, ctx := openStore(t)
	conn := rawConn(t, ctx)
	// Pin the shared-catalog owner for this test: it is the FIRST admin, which
	// in a shared DB may be some other test's admin. Seed services under it.
	owner, err := store.SharedCatalogOwnerID(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		owner = seedStoreAdmin(t, store, ctx)
	} else {
		require.NoError(t, err)
	}
	a := seedStoreService(t, store, ctx, owner)
	b := seedStoreService(t, store, ctx, owner)
	plain := seedStoreUser(t, store, ctx)
	notShared := seedStoreService(t, store, ctx, plain)

	kid, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)

	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{a, b, a}))
	ids, err := store.KidServiceIDs(ctx, kid.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{a, b}, ids)

	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{b}))
	ids, err = store.KidServiceIDs(ctx, kid.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{b}, ids, "whole-set replace drops a")

	for _, bad := range [][]string{
		{a, "00000000-0000-0000-0000-000000000000"},
		{a, "not-a-uuid"},
		{a, notShared},
	} {
		err := store.SetKidServices(ctx, kid.ID, bad)
		assert.ErrorIs(t, err, storage.ErrUnknownService, "ids %v", bad)
		ids, err = store.KidServiceIDs(ctx, kid.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{b}, ids, "a rejected replace leaves the old set intact")
	}

	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{}))
	ids, err = store.KidServiceIDs(ctx, kid.ID)
	require.NoError(t, err)
	assert.Empty(t, ids, "an empty set is allowed (the kid sees an empty dashboard)")

	// Only a kid can hold an allow-list.
	assert.ErrorIs(t, store.SetKidServices(ctx, plain, []string{a}), storage.ErrNotFound)
	var n int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM kid_allowed_services WHERE user_id = $1`, plain).Scan(&n))
	assert.Zero(t, n)
}

// Deleting a service drops it from every allow-list (FK cascade, spec §6).
func TestDeleteService_DropsFromAllowLists(t *testing.T) {
	store, ctx := openStore(t)
	owner, err := store.SharedCatalogOwnerID(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		owner = seedStoreAdmin(t, store, ctx)
	} else {
		require.NoError(t, err)
	}
	a := seedStoreService(t, store, ctx, owner)
	b := seedStoreService(t, store, ctx, owner)
	k1, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	k2, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	require.NoError(t, store.SetKidServices(ctx, k1.ID, []string{a, b}))
	require.NoError(t, store.SetKidServices(ctx, k2.ID, []string{a}))

	require.NoError(t, store.DeleteService(ctx, a, owner))

	ids, err := store.KidServiceIDs(ctx, k1.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{b}, ids)
	ids, err = store.KidServiceIDs(ctx, k2.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// AC-014 — deleting a kid removes its sessions, favorites, preferences (layout,
// collapsed sections) and allow-list rows. DeleteKid refuses to delete an
// adult.
func TestDeleteKid_CascadesAndRefusesAdults(t *testing.T) {
	store, ctx := openStore(t)
	conn := rawConn(t, ctx)
	owner, err := store.SharedCatalogOwnerID(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		owner = seedStoreAdmin(t, store, ctx)
	} else {
		require.NoError(t, err)
	}
	svc := seedStoreService(t, store, ctx, owner)
	cat, err := store.CreateCategory(ctx, owner, uniqueName(t))
	require.NoError(t, err)

	kid, err := store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{svc}))
	require.NoError(t, store.AddFavorite(ctx, kid.ID, svc))
	require.NoError(t, store.SetLayout(ctx, kid.ID, []string{svc}))
	require.NoError(t, store.SetCollapsedCategories(ctx, kid.ID, []string{cat.ID}))
	_, err = conn.Exec(ctx,
		`INSERT INTO sessions (token_hash, handle, user_id, expires_at) VALUES (sha256($1::bytea), $2, $3, now() + interval '1 day')`,
		[]byte("kid-cascade-token-"+kid.ID), "h-"+kid.ID, kid.ID)
	require.NoError(t, err)

	tables := []string{"kid_allowed_services", "favorites", "user_layout", "user_collapsed_categories", "sessions"}
	for _, tbl := range tables {
		var n int
		require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, kid.ID).Scan(&n))
		require.Equal(t, 1, n, "precondition: %s has the kid's row", tbl)
	}

	// An adult id is not a kid: refused, nothing deleted.
	assert.ErrorIs(t, store.DeleteKid(ctx, owner), storage.ErrNotFound)
	_, err = store.UserByID(ctx, owner)
	require.NoError(t, err, "DeleteKid must never delete an adult")
	assert.ErrorIs(t, store.DeleteKid(ctx, "not-a-uuid"), storage.ErrNotFound)

	require.NoError(t, store.DeleteKid(ctx, kid.ID))
	for _, tbl := range tables {
		var n int
		require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, kid.ID).Scan(&n))
		assert.Zero(t, n, "AC-014: %s rows are removed with the kid", tbl)
	}
	_, err = store.UserByID(ctx, kid.ID)
	assert.ErrorIs(t, err, storage.ErrNotFound)
	assert.ErrorIs(t, store.DeleteKid(ctx, kid.ID), storage.ErrNotFound, "a second delete is not found")
}
