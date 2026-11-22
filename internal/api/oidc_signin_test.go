package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-signin-hardening §2.4, §2.5 — where a failed single sign-on lands, and
// who single sign-on may make an account for.

const (
	adminGroup  = "homepad-admins"
	memberGroup = "homepad"
)

func withMemberGroup(c oidc.Config, g string) oidc.Config {
	c.MemberGroup = g
	return c
}

func inviteOnly() *api.RegistrationMode { return modePtr(api.RegistrationClosed) }

func openMode() *api.RegistrationMode { return modePtr(api.RegistrationOpen) }

// signinError asserts resp is the §2.4 landing (302 to /#signin-error=<code>)
// with no session cookie, and returns the code.
func signinError(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "a refused sign-in redirects, it never renders a text page")
	for _, c := range resp.Cookies() {
		if c.Name == "homepad_session" && c.MaxAge >= 0 && c.Value != "" {
			t.Errorf("a refused sign-in sets no session cookie")
		}
	}
	loc := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(loc, "/#signin-error="), "Location = %q", loc)
	return strings.TrimPrefix(loc, "/#signin-error=")
}

// noRedirect is a client that surfaces each redirect.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// AC-010, TC-006 — under invite_only a new identity gets an account only
// through OIDC_ADMIN_GROUP (admin) or OIDC_MEMBER_GROUP (member).
func TestOIDC_InviteOnly_NewIdentities(t *testing.T) {
	cases := []struct {
		name     string
		groups   []string
		member   string
		wantRole string // "" = refused with not_invited
	}{
		{"admin group", []string{"everyone", adminGroup}, memberGroup, "admin"},
		{"member group", []string{"everyone", memberGroup}, memberGroup, "user"},
		{"both groups", []string{memberGroup, adminGroup}, memberGroup, "admin"},
		{"neither group", []string{"everyone"}, memberGroup, ""},
		{"no groups at all", nil, memberGroup, ""},
		{"member group unset", []string{"everyone", memberGroup}, "", ""},
		{"group names match exactly", []string{"Homepad"}, memberGroup, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := captureLog(t)
			idp := newMockIdP(t, "pat@example.com", c.groups)
			homepad, store := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), c.member),
				testsupport.Options{Registration: inviteOnly()})
			ctx := context.Background()
			_, err := store.CreateUser(ctx, "caleb@example.com", "x", "admin")
			require.NoError(t, err)

			if c.wantRole != "" {
				cookie := runOIDCLogin(t, homepad, idp)
				email, role := fetchMe(t, homepad, cookie)
				assert.Equal(t, "pat@example.com", email)
				assert.Equal(t, c.wantRole, role)
				return
			}
			assert.Equal(t, "not_invited", signinError(t, oidcCallback(t, homepad, idp)))
			_, err = store.UserByEmail(ctx, "pat@example.com")
			assert.Error(t, err, "no account is created")
			assert.Contains(t, buf.String(), "oidc: refused new identity under invite_only (not in an allowed group)")
			assert.NotContains(t, buf.String(), "pat@example.com", "the email is not logged")
		})
	}
}

// TC-006 — Pat is refused, is added to the member group, and signs in.
func TestOIDC_InviteOnly_AddedToGroupThenSignsIn(t *testing.T) {
	idp := newMockIdP(t, "pat@example.com", []string{"everyone"})
	homepad, store := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), memberGroup),
		testsupport.Options{Registration: inviteOnly()})
	_, err := store.CreateUser(context.Background(), "caleb@example.com", "x", "admin")
	require.NoError(t, err)

	assert.Equal(t, "not_invited", signinError(t, oidcCallback(t, homepad, idp)))
	idp.groups = []string{"everyone", memberGroup}
	cookie := runOIDCLogin(t, homepad, idp)
	_, role := fetchMe(t, homepad, cookie)
	assert.Equal(t, "user", role)
}

// §2.5 — an empty install under invite_only gets its first account from
// single sign-on only through OIDC_ADMIN_GROUP, so it never ends up with
// only a member.
func TestOIDC_InviteOnly_EmptyInstall(t *testing.T) {
	t.Run("member group is refused", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", []string{memberGroup})
		homepad, store := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), memberGroup),
			testsupport.Options{Registration: inviteOnly()})
		assert.Equal(t, "not_invited", signinError(t, oidcCallback(t, homepad, idp)))
		n, err := store.CountUsers(context.Background())
		require.NoError(t, err)
		assert.Zero(t, n)
	})
	t.Run("admin group sets it up", func(t *testing.T) {
		idp := newMockIdP(t, "caleb@example.com", []string{adminGroup})
		homepad, _ := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), memberGroup),
			testsupport.Options{Registration: inviteOnly()})
		cookie := runOIDCLogin(t, homepad, idp)
		_, role := fetchMe(t, homepad, cookie)
		assert.Equal(t, "admin", role)
	})
}

// AC-011, TC-007 — under invite_only an identity whose verified email matches
// an existing adult signs in as that adult, whatever its groups.
func TestOIDC_InviteOnly_ExistingAdultSignsIn(t *testing.T) {
	for _, groups := range [][]string{nil, {"everyone"}, {memberGroup}} {
		t.Run(strings.Join(groups, ","), func(t *testing.T) {
			idp := newMockIdP(t, "jo@example.com", groups)
			homepad, store := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), memberGroup),
				testsupport.Options{Registration: inviteOnly()})
			hash, err := bcrypt.GenerateFromPassword([]byte("jo's password"), bcrypt.MinCost)
			require.NoError(t, err)
			ctx := context.Background()
			_, err = store.CreateUser(ctx, "caleb@example.com", "x", "admin")
			require.NoError(t, err)
			_, err = store.CreateUser(ctx, "jo@example.com", string(hash), "user")
			require.NoError(t, err)

			cookie := runOIDCLogin(t, homepad, idp)
			email, role := fetchMe(t, homepad, cookie)
			assert.Equal(t, "jo@example.com", email)
			assert.Equal(t, "user", role)
			n, err := store.CountUsers(ctx)
			require.NoError(t, err)
			assert.Equal(t, 2, n, "no account is created")
		})
	}
}

// AC-012 — under open, creation is unchanged from 17.1.0: the admin group
// makes an admin, anyone else a member, the member group included.
func TestOIDC_Open_CreationUnchanged(t *testing.T) {
	for _, c := range []struct {
		groups   []string
		wantRole string
	}{
		{[]string{adminGroup}, "admin"},
		{[]string{memberGroup}, "user"},
		{[]string{"everyone"}, "user"},
		{nil, "user"},
	} {
		t.Run(strings.Join(c.groups, ","), func(t *testing.T) {
			idp := newMockIdP(t, "pat@example.com", c.groups)
			homepad, store := testsupport.NewOIDCServerWith(t, withMemberGroup(idp.config(adminGroup), memberGroup),
				testsupport.Options{Registration: openMode()})
			_, err := store.CreateUser(context.Background(), "caleb@example.com", "x", "admin")
			require.NoError(t, err)
			cookie := runOIDCLogin(t, homepad, idp)
			_, role := fetchMe(t, homepad, cookie)
			assert.Equal(t, c.wantRole, role)
		})
	}
	// An empty open install: the first identity outside the admin group is a
	// member, as in 17.1.0.
	idp := newMockIdP(t, "first@example.com", []string{"everyone"})
	homepad, _ := testsupport.NewOIDCServerWith(t, idp.config(adminGroup), testsupport.Options{Registration: openMode()})
	_, role := fetchMe(t, homepad, runOIDCLogin(t, homepad, idp))
	assert.Equal(t, "user", role)
}

// AC-002 — every callback failure redirects to /#signin-error=<code>.
func TestOIDCCallback_FailuresLand(t *testing.T) {
	t.Run("missing state or code", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", nil)
		homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
		for _, q := range []string{"", "?state=x", "?code=y"} {
			resp, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/callback" + q)
			require.NoError(t, err)
			assert.Equal(t, "sso_failed", signinError(t, resp), q)
		}
	})
	t.Run("unknown or reused state", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", nil)
		homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
		resp, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/callback?state=nope&code=x")
		require.NoError(t, err)
		assert.Equal(t, "sso_failed", signinError(t, resp))
	})
	t.Run("token exchange fails", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", nil)
		homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
		start, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/login")
		require.NoError(t, err)
		start.Body.Close()
		au, err := url.Parse(start.Header.Get("Location"))
		require.NoError(t, err)
		resp, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/callback?code=not-a-code&state=" + url.QueryEscape(au.Query().Get("state")))
		require.NoError(t, err)
		assert.Equal(t, "sso_failed", signinError(t, resp))
	})
	t.Run("no email", func(t *testing.T) {
		idp := newMockIdP(t, "", nil)
		homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
		assert.Equal(t, "sso_failed", signinError(t, oidcCallback(t, homepad, idp)))
	})
	t.Run("email not verified", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", nil)
		idp.verified, idp.uiVerified = false, false
		homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
		assert.Equal(t, "email_unverified", signinError(t, oidcCallback(t, homepad, idp)))
	})
	t.Run("account limit", func(t *testing.T) {
		idp := newMockIdP(t, "pat@example.com", nil)
		homepad, store := testsupport.NewOIDCServerWith(t, idp.config(""), testsupport.Options{MaxAccounts: 1})
		_, err := store.CreateUser(context.Background(), "caleb@example.com", "x", "admin")
		require.NoError(t, err)
		assert.Equal(t, "account_limit", signinError(t, oidcCallback(t, homepad, idp)))
	})
}

// AC-019 — the start route's own limit (20 a minute, burst 10) lands on the
// sign-in screen with too_many instead of a text page.
func TestOIDCLogin_TooManyRedirects(t *testing.T) {
	idp := newMockIdP(t, "pat@example.com", nil)
	homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))
	for i := 0; i < 10; i++ {
		resp, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/login")
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		require.True(t, strings.HasPrefix(resp.Header.Get("Location"), idp.issuer()), "start %d goes to the provider", i+1)
	}
	resp, err := noRedirect.Get(homepad.URL + "/api/auth/oidc/login")
	require.NoError(t, err)
	assert.Equal(t, "too_many", signinError(t, resp))
}

// AC-013 — OIDC_MEMBER_GROUP is read from the environment.
func TestOIDCConfigFromEnv_MemberGroup(t *testing.T) {
	t.Setenv("OIDC_MEMBER_GROUP", "homepad")
	t.Setenv("OIDC_ADMIN_GROUP", "homepad-admins")
	c := oidc.ConfigFromEnv()
	assert.Equal(t, "homepad", c.MemberGroup)
	assert.Equal(t, "homepad-admins", c.AdminGroup)
}
