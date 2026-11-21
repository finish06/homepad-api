package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-password-reset-link AC-016, TC-006 — the lone admin's way back in:
// `homepad-api reset-link <email>` prints a one-time /#reset= path and its
// expiry, and writes the same audit line as the admin route with cli as the
// actor. The token goes to stdout only.

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

var resetPath = regexp.MustCompile(`^/#reset=([A-Za-z0-9_-]{43})$`)

func TestResetLinkCommand_PrintsALinkThatWorks(t *testing.T) {
	logs := captureLog(t)
	env := testsupport.NewEnv(t)
	var stdout, stderr bytes.Buffer
	code := runResetLink(context.Background(), env.DSN, []string{"admin@homepad.test"}, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2, stdout.String())
	m := resetPath.FindStringSubmatch(lines[0])
	require.NotNil(t, m, "the first line is the /#reset= path: %q", lines[0])
	token := m[1]

	var id string
	var expires time.Time
	var createdBy *string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT id::text, expires_at, created_by::text FROM password_resets WHERE user_id = $1`, env.AdminID).Scan(&id, &expires, &createdBy))
	assert.Nil(t, createdBy, "the CLI is no account")
	assert.Contains(t, stdout.String(), expires.UTC().Format("2006-01-02 15:04 UTC"), "the expiry is printed")
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), expires, time.Minute)

	assert.NotContains(t, stderr.String(), token, "the token goes to stdout only")
	assert.NotContains(t, logs.String(), token, "and never to the log")
	assert.Contains(t, logs.String(), "reset: admin cli created reset link "+id+" for user "+env.AdminID+" expires=")
	assert.NotContains(t, logs.String(), "admin@homepad.test", "no email in the log")

	// The printed link works like one an admin made (TC-006).
	body, _ := json.Marshal(map[string]string{"token": token})
	r, err := http.Post(env.URL+"/api/password-resets/lookup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	var v struct {
		Email string `json:"email"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&v))
	assert.Equal(t, "admin@homepad.test", v.Email)

	// A second run revokes the first link (Q6), as the route does.
	stdout.Reset()
	require.Equal(t, 0, runResetLink(context.Background(), env.DSN, []string{"admin@homepad.test"}, &stdout, &stderr))
	assert.Contains(t, logs.String(), "reset: link "+id+" revoked by a newer link")
}

func TestResetLinkCommand_Refusals(t *testing.T) {
	env := testsupport.NewEnv(t)
	ctx := context.Background()
	env.SeedKid(t, "Maya")
	sso, err := env.Store.CreateUser(ctx, "pat@example.com", "oidc:no-local-password", "user")
	require.NoError(t, err)
	require.NoError(t, env.Sessions.Bind(ctx, "pat-session", sso.ID, session.AdultLifetime))

	for name, c := range map[string]struct {
		args []string
		code int
		says string
	}{
		"unknown email": {[]string{"nobody@example.com"}, 1, "No adult account"},
		"a kid's name":  {[]string{"Maya"}, 1, "No adult account"},
		"SSO only":      {[]string{"pat@example.com"}, 1, "single sign-on"},
		"no email":      {nil, 2, "usage: homepad-api reset-link <email>"},
		"two emails":    {[]string{"a@example.com", "b@example.com"}, 2, "usage: homepad-api reset-link <email>"},
	} {
		var stdout, stderr bytes.Buffer
		code := runResetLink(ctx, env.DSN, c.args, &stdout, &stderr)
		assert.Equal(t, c.code, code, name)
		assert.Empty(t, stdout.String(), "%s: nothing on stdout", name)
		assert.Contains(t, stderr.String(), c.says, name)
	}

	var n int
	require.NoError(t, env.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM password_resets`).Scan(&n))
	assert.Zero(t, n, "no refusal made a link")

	var stdout, stderr bytes.Buffer
	assert.Equal(t, 1, runResetLink(ctx, "", []string{"admin@homepad.test"}, &stdout, &stderr), "no DATABASE_URL")
	assert.Contains(t, stderr.String(), "DATABASE_URL")
}
