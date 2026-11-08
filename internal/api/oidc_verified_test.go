package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// homepad-api #89 — the OIDC callback links a sign-in to a local account by
// email, and creates an account from the email claim. Both now require the
// identity provider to say the email is verified (email_verified == true).
// An unverified or missing claim links nothing, creates nothing and starts no
// session.

func TestOIDCCallback_EmailVerified(t *testing.T) {
	const email = "victim@example.com"
	hash, err := bcrypt.GenerateFromPassword([]byte("local-pw"), bcrypt.MinCost)
	require.NoError(t, err)

	type tc struct {
		name       string
		verified   any // ID token claim; nil = absent
		uiVerified any // userinfo claim; nil = absent
		wantOK     bool
	}
	cases := []tc{
		{"verified true", true, true, true},
		{"verified as the string true", "true", nil, true},
		{"unverified", false, false, false},
		{"unverified as the string false", "false", nil, false},
		{"missing everywhere", nil, nil, false},
		{"missing in the ID token, verified in userinfo", nil, true, true},
		{"missing in the ID token, unverified in userinfo", nil, false, false},
		{"not a boolean", "yes", nil, false},
	}

	for _, c := range cases {
		t.Run("link existing admin/"+c.name, func(t *testing.T) {
			idp := newMockIdP(t, email, []string{"everyone"})
			idp.verified, idp.uiVerified = c.verified, c.uiVerified
			homepad, store := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))
			ctx := context.Background()
			existing, err := store.CreateUser(ctx, email, string(hash), "admin")
			require.NoError(t, err)

			if c.wantOK {
				cookie := runOIDCLogin(t, homepad, idp)
				gotEmail, role := fetchMe(t, homepad, cookie)
				assert.Equal(t, email, gotEmail)
				assert.Equal(t, "admin", role, "a verified email links the existing account")
				return
			}
			code := runOIDCLoginStatus(t, homepad, idp)
			assert.Equal(t, http.StatusForbidden, code, "an unverified email must not sign in as %s", existing.Email)
			n, err := store.CountUsers(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "nothing created")
		})

		t.Run("create new account/"+c.name, func(t *testing.T) {
			idp := newMockIdP(t, "newcomer@example.com", []string{"everyone"})
			idp.verified, idp.uiVerified = c.verified, c.uiVerified
			homepad, store := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))
			ctx := context.Background()

			if c.wantOK {
				cookie := runOIDCLogin(t, homepad, idp)
				gotEmail, _ := fetchMe(t, homepad, cookie)
				assert.Equal(t, "newcomer@example.com", gotEmail)
				return
			}
			code := runOIDCLoginStatus(t, homepad, idp)
			assert.Equal(t, http.StatusForbidden, code)
			_, err := store.UserByEmail(ctx, "newcomer@example.com")
			assert.Error(t, err, "no account is created from an unverified email")
		})
	}
}

// When the ID token carries no email at all, the userinfo email is used, and
// it must bring its own email_verified: the ID token's claim (if any) was
// about no email.
func TestOIDCCallback_UserinfoEmailNeedsItsOwnVerification(t *testing.T) {
	for name, uiVerified := range map[string]any{"verified": true, "unverified": false, "missing": nil} {
		t.Run(name, func(t *testing.T) {
			idp := newMockIdP(t, "fromuserinfo@example.com", []string{"everyone"})
			idp.idTokenEmail = false
			idp.uiVerified = uiVerified
			homepad, store := testsupport.NewOIDCServer(t, idp.config(""))
			if uiVerified == true {
				cookie := runOIDCLogin(t, homepad, idp)
				gotEmail, _ := fetchMe(t, homepad, cookie)
				assert.Equal(t, "fromuserinfo@example.com", gotEmail)
				return
			}
			assert.Equal(t, http.StatusForbidden, runOIDCLoginStatus(t, homepad, idp))
			_, err := store.UserByEmail(context.Background(), "fromuserinfo@example.com")
			assert.Error(t, err)
		})
	}
}

// A userinfo email_verified only counts for the email it describes: when the
// ID token has an email but no claim, a userinfo answer about a different
// address does not verify it.
func TestOIDCCallback_UserinfoVerificationMustMatchEmail(t *testing.T) {
	idp := newMockIdP(t, "token@example.com", []string{"everyone"})
	idp.verified = nil
	homepad, store := testsupport.NewOIDCServer(t, idp.config(""))
	// Userinfo vouches for a different address than the token's.
	idp.uiEmail = "other@example.com"
	assert.Equal(t, http.StatusForbidden, runOIDCLoginStatus(t, homepad, idp))
	_, err := store.UserByEmail(context.Background(), "token@example.com")
	assert.Error(t, err)
}
