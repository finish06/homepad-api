package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Login does the same bcrypt work whether or not the email has an account,
// so response time does not say which emails exist. The compare is counted,
// not timed: every path runs exactly one, and the paths with no usable hash
// run it against the fixed dummy hash.
func TestLoginPasswordOK_RunsOneCompareOnEveryPath(t *testing.T) {
	const pw = "correct horse battery"
	realHash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// A 72-byte password, and the same password with one more byte. bcrypt
	// reads only the first 72 bytes, so without a length check the longer
	// one would match the shorter one's hash.
	pw72 := strings.Repeat("é", 36)
	hash72, err := bcrypt.GenerateFromPassword([]byte(pw72), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	adult := storage.User{ID: "u1", Email: "a@example.com", Role: "user", PasswordHash: string(realHash)}
	cases := []struct {
		name      string
		u         storage.User
		lookupErr error
		password  string
		wantOK    bool
		wantDummy bool
	}{
		{"unknown email", storage.User{}, storage.ErrNotFound, pw, false, true},
		{"lookup failed", storage.User{}, errors.New("db down"), pw, false, true},
		{"kid (no hash)", storage.User{ID: "k1", Role: "kid"}, nil, pw, false, true},
		{"adult with no hash", storage.User{ID: "u2", Role: "user"}, nil, pw, false, true},
		{"OIDC-only adult", storage.User{ID: "u3", Role: "admin", PasswordHash: oidcNoPassword}, nil, pw, false, true},
		{"over 72 bytes", storage.User{ID: "u4", Role: "user", PasswordHash: string(hash72)}, nil, pw72 + "x", false, true},
		{"right password", adult, nil, pw, true, false},
		{"wrong password", adult, nil, "wrong password!", false, false},
		{"72-byte password", storage.User{ID: "u4", Role: "user", PasswordHash: string(hash72)}, nil, pw72, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var gotHash []byte
			s := &server{comparePassword: func(hash, password []byte) error {
				calls++
				gotHash = hash
				return bcrypt.CompareHashAndPassword(hash, password)
			}}
			ok := s.loginPasswordOK(tc.u, tc.lookupErr, tc.password)
			if ok != tc.wantOK {
				t.Errorf("loginPasswordOK = %v, want %v", ok, tc.wantOK)
			}
			if calls != 1 {
				t.Fatalf("bcrypt compare ran %d times, want exactly 1", calls)
			}
			if isDummy := bytes.Equal(gotHash, []byte(dummyPasswordHash)); isDummy != tc.wantDummy {
				t.Errorf("compared against the dummy hash = %v, want %v", isDummy, tc.wantDummy)
			}
		})
	}
}

// With no compare injected, the real bcrypt compare is used.
func TestLoginPasswordOK_DefaultsToBcrypt(t *testing.T) {
	const pw = "correct horse battery"
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	s := &server{}
	u := storage.User{ID: "u1", Role: "admin", PasswordHash: string(h)}
	if !s.loginPasswordOK(u, nil, pw) {
		t.Error("the right password must sign in")
	}
	if s.loginPasswordOK(u, nil, "not the password") {
		t.Error("a wrong password must not sign in")
	}
	if s.loginPasswordOK(storage.User{}, storage.ErrNotFound, pw) {
		t.Error("an unknown email must not sign in")
	}
}

// The dummy hash costs what a real one does, or the unknown-email path is
// still the fast one.
func TestDummyPasswordHash_IsDefaultCost(t *testing.T) {
	cost, err := bcrypt.Cost([]byte(dummyPasswordHash))
	if err != nil {
		t.Fatalf("dummy hash is not a bcrypt hash: %v", err)
	}
	if cost != bcrypt.DefaultCost {
		t.Fatalf("dummy hash cost = %d, want bcrypt.DefaultCost (%d), the cost new passwords are hashed at", cost, bcrypt.DefaultCost)
	}
}

// bcrypt reads at most 72 bytes. The limit is in bytes, not characters: a
// password of 25 characters can already be over it.
func TestPasswordTooLong_CountsBytes(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		want bool
	}{
		{"72 ASCII bytes", strings.Repeat("a", 72), false},
		{"73 ASCII bytes", strings.Repeat("a", 73), true},
		{"36 two-byte runes = 72 bytes", strings.Repeat("é", 36), false},
		{"36 two-byte runes + 1 = 73 bytes", strings.Repeat("é", 36) + "a", true},
		{"24 three-byte runes = 72 bytes", strings.Repeat("€", 24), false},
		{"24 three-byte runes + 1 = 73 bytes (25 runes)", strings.Repeat("€", 24) + "a", true},
		{"18 four-byte runes = 72 bytes", strings.Repeat("🔑", 18), false},
		{"18 four-byte runes + 1 = 73 bytes", strings.Repeat("🔑", 18) + "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want != (len(tc.pw) > 72) {
				t.Fatalf("bad fixture: %q is not over 72 bytes", tc.pw)
			}
			if got := passwordTooLong(tc.pw); got != tc.want {
				t.Errorf("passwordTooLong(%d bytes, %d runes) = %v, want %v",
					len(tc.pw), utf8.RuneCountInString(tc.pw), got, tc.want)
			}
		})
	}
}

// Every session cookie comes from one place and follows COOKIE_SECURE: the
// one startSession sets (password login, invite accept, OIDC callback) and
// the cleared one logout sets.
func TestSessionCookies_FollowCookieSecure(t *testing.T) {
	for _, secure := range []bool{true, false} {
		s := &server{sessions: session.NewMemoryStore(), cookieSecure: secure}

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if !s.startSession(w, r, storage.User{ID: "u1", Role: "user"}) {
			t.Fatalf("startSession failed: %d %s", w.Code, w.Body.String())
		}
		checkCookie(t, "startSession", w.Result().Cookies(), secure)

		w = httptest.NewRecorder()
		s.handleLogout(w, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
		checkCookie(t, "logout", w.Result().Cookies(), secure)
	}
}

func checkCookie(t *testing.T, which string, cookies []*http.Cookie, wantSecure bool) {
	t.Helper()
	for _, c := range cookies {
		if c.Name != sessionCookie {
			continue
		}
		if c.Secure != wantSecure {
			t.Errorf("%s: Secure = %v, want %v (COOKIE_SECURE)", which, c.Secure, wantSecure)
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("%s: cookie must be HttpOnly, SameSite=Lax, Path=/: %+v", which, c)
		}
		return
	}
	t.Errorf("%s: no %s cookie set", which, sessionCookie)
}
