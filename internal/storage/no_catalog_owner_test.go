package storage_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-household-catalog-owner AC-004 — no Go code outside migrations/ refers
// to a catalog owner. The named helpers are gone, and no SQL in internal/
// filters or writes services.user_id or categories.user_id (both columns are
// dropped by 0021, so such a query would fail at run time anyway; this finds
// it before a request does).

var goneOwnerNames = []string{"SharedCatalogOwnerID", "sharedOwnerSQL", "catalogOwner", "NonOwnerAdminCatalogRows", "requireCatalogAdmin"}

var (
	sqlVerb = regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE)\b`)
	// A table reference, with its alias when it has one.
	tableRef = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|UPDATE|INTO)\s+([a-z_]+)(?:\s+(?:AS\s+)?([a-z_]+))?`)
	// An INSERT column list that names user_id.
	insertOwner = regexp.MustCompile(`(?i)\bINTO\s+(services|categories)\s*\([^)]*\buser_id\b`)
	qualified   = regexp.MustCompile(`(?i)\b([a-z_]+)\.user_id\b`)
	unqualified = regexp.MustCompile(`(?i)(^|[^.\w])user_id\b`)
	sqlKeywords = map[string]bool{"where": true, "on": true, "set": true, "left": true, "join": true, "order": true,
		"group": true, "limit": true, "values": true, "returning": true, "inner": true, "using": true, "with": true}
)

// ownerSQL reports why sql reads or writes a catalog owner, or "".
func ownerSQL(sql string) string {
	if m := insertOwner.FindString(sql); m != "" {
		return "inserts an owner: " + m
	}
	catalogAlias := map[string]bool{}
	other := false
	for _, m := range tableRef.FindAllStringSubmatch(sql, -1) {
		table, alias := strings.ToLower(m[1]), strings.ToLower(m[2])
		if table != "services" && table != "categories" {
			other = true
			continue
		}
		catalogAlias[table] = true
		if alias != "" && !sqlKeywords[alias] {
			catalogAlias[alias] = true
		}
	}
	if len(catalogAlias) == 0 {
		return ""
	}
	for _, m := range qualified.FindAllStringSubmatch(sql, -1) {
		if catalogAlias[strings.ToLower(m[1])] {
			return "uses " + m[0]
		}
	}
	if !other && unqualified.MatchString(sql) {
		return "uses user_id on a catalog table"
	}
	return ""
}

// The detector must catch the queries 17.1.0 ran, and pass the per-person
// ones that keep a user_id of their own.
func TestOwnerSQLDetector(t *testing.T) {
	for _, bad := range []string{
		`SELECT id, name FROM categories WHERE user_id = $1 ORDER BY sort_index`,
		`SELECT s.id FROM services s LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $2 WHERE s.user_id = $1`,
		`INSERT INTO services (user_id, slug, name) VALUES ($1, $2, $3)`,
		`UPDATE categories SET name = $3 WHERE id = $1 AND user_id = $2`,
		`DELETE FROM services WHERE id = $1 AND user_id = $2`,
		`SELECT count(*) FROM kid_allowed_services k JOIN services sv ON sv.id = k.service_id WHERE sv.user_id = $1`,
		`UPDATE services s SET source_library_id = NULL WHERE s.user_id = $1`,
	} {
		assert.NotEmpty(t, ownerSQL(bad), bad)
	}
	for _, good := range []string{
		`SELECT category_id FROM user_collapsed_categories WHERE user_id = $1`,
		`INSERT INTO user_layout (user_id, service_id, sort_index) SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM services WHERE id = $2)`,
		`SELECT s.id FROM services s LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $1 ORDER BY ul.sort_index`,
		`SELECT k.service_id FROM kid_allowed_services k JOIN services sv ON sv.id = k.service_id WHERE k.user_id = $1`,
		`INSERT INTO favorites (user_id, service_id) SELECT $1, s.id FROM services s WHERE s.id::text = ANY($2::text[])`,
		`SELECT id, slug FROM services ORDER BY slug`,
	} {
		assert.Empty(t, ownerSQL(good), good)
	}
}

func TestNoCatalogOwnerInGoCode(t *testing.T) {
	var files []string
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		}))
	}
	require.NotEmpty(t, files)

	fset := token.NewFileSet()
	for _, path := range files {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		require.NoError(t, err, path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				for _, gone := range goneOwnerNames {
					if x.Name == gone {
						t.Errorf("%s: %s is still referenced", fset.Position(x.Pos()), gone)
					}
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(x.Value)
				if err != nil || !sqlVerb.MatchString(s) {
					return true
				}
				if why := ownerSQL(s); why != "" {
					t.Errorf("%s: SQL %s", fset.Position(x.Pos()), why)
				}
			}
			return true
		})
	}
}
