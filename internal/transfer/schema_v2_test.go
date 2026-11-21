package transfer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-household-catalog-owner AC-012 (Q5) — 18.0.0 writes schemaVersion 2
// with the same keys as 17.1.0's version 1. The 1→2 upgrader changes only the
// number, so a 17.1.0 file restores exactly as it did on 17.1.0.

func TestSchemaVersion_IsTwo(t *testing.T) {
	assert.Equal(t, 2, transfer.SchemaVersion)
	out := string(transfer.Encode(transfer.BuildFile(populatedState(), true, "18.0.0", exportTime)))
	assert.True(t, strings.HasPrefix(out, "{\n  \"format\": \"homepad-backup\",\n  \"schemaVersion\": 2,\n"), out[:80])
}

// The keys of a v2 export are exactly the keys of a v1 export: no field is
// added or removed.
func TestSchemaVersion_V2HasTheSameKeysAsV1(t *testing.T) {
	keys := func(raw []byte) []string {
		var v any
		require.NoError(t, json.Unmarshal(raw, &v))
		var out []string
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, e := range x {
					out = append(out, path+"."+k)
					walk(path+"."+k, e)
				}
			case []any:
				for _, e := range x {
					walk(path+"[]", e)
				}
			}
		}
		walk("$", v)
		return out
	}
	v1 := keys(readFixture(t, "backups/v1.json"))
	v2 := keys(transfer.Encode(decodeFixtureVersion(t, 1).File))
	assert.ElementsMatch(t, uniq(v1), uniq(v2))
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// The upgrader changes the number and nothing else: the v1 fixture decodes to
// the v2 fixture, apart from the Homepad version that wrote it.
func TestUpgrade_1To2ChangesOnlyTheNumber(t *testing.T) {
	v1 := decodeFixtureVersion(t, 1)
	v2 := decodeFixtureVersion(t, 2)
	assert.Equal(t, 2, v1.File.SchemaVersion, "upgraded on the way in")
	assert.Equal(t, "17.1.0", v1.File.HomepadVersion, "homepadVersion is for people; nothing rewrites it")
	assert.Equal(t, v2.UnknownFields, v1.UnknownFields)
	f := *v1.File
	f.HomepadVersion = v2.File.HomepadVersion
	assert.Equal(t, *v2.File, f)
}

// A v1 file gets the same plan as the same file at v2, in both modes, on an
// empty install and on a populated one.
func TestUpgrade_V1PlansLikeV2(t *testing.T) {
	states := map[string]func() *storage.TransferState{"fresh": freshState, "populated": populatedState}
	for _, mode := range []string{transfer.ModeMerge, transfer.ModeReplace} {
		for name, state := range states {
			t.Run(mode+"/"+name, func(t *testing.T) {
				p1 := transfer.PlanRestore(state(), decodeFixtureVersion(t, 1), mode)
				p2 := transfer.PlanRestore(state(), decodeFixtureVersion(t, 2), mode)
				assert.Equal(t, p2.Summary, p1.Summary)
				assert.Equal(t, p2.Items, p1.Items)
				assert.Equal(t, p2.Warnings, p1.Warnings)
				assert.Equal(t, p2.Ignored, p1.Ignored)
				assert.Equal(t, p2.PlanHash, p1.PlanHash, "the same plan, so the same hash")
			})
		}
	}
}
