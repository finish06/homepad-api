package transfer

import "gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"

// SPEC-backup-import §2.2, §2.4, §7, §8 — the plan.

// Modes.
const (
	ModeMerge   = "merge"
	ModeReplace = "replace"
	ModeImport  = "import"
)

// Actions.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionDelete    = "delete"
	ActionSkip      = "skip"
)

// Kinds.
const (
	KindSection  = "section"
	KindApp      = "app"
	KindIcon     = "icon"
	KindOffer    = "offer"
	KindSettings = "settings"
	KindAdult    = "adult"
	KindKid      = "kid"
)

// Plan is what a preview returns and what an apply carries out (§8).
type Plan struct {
	PlanHash      string        `json:"planHash,omitempty"`
	Mode          string        `json:"mode"`
	Summary       Summary       `json:"summary"`
	Items         []Item        `json:"items"`
	Warnings      []PlanWarning `json:"warnings"`
	Ignored       Ignored       `json:"ignored"`
	GatusSnippet  string        `json:"gatusSnippet,omitempty"`
	OtherAccounts int           `json:"otherAccounts"`
	Applied       bool          `json:"applied,omitempty"`

	ops *ops
}

// Summary is the counts line.
type Summary struct {
	Create    CreateCounts `json:"create"`
	Update    UpdateCounts `json:"update"`
	Delete    DeleteCounts `json:"delete"`
	Unchanged int          `json:"unchanged"`
	Skipped   int          `json:"skipped"`
}

// CreateCounts counts creates.
type CreateCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Icons    int `json:"icons"`
	Offers   int `json:"offers"`
	Kids     int `json:"kids"`
}

// UpdateCounts counts updates.
type UpdateCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Icons    int `json:"icons"`
	Offers   int `json:"offers"`
	People   int `json:"people"`
	Settings int `json:"settings"`
}

// DeleteCounts counts Replace's deletions.
type DeleteCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Offers   int `json:"offers"`
}

// Item is one row of the preview.
type Item struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Action   string   `json:"action"`
	Section  *string  `json:"section,omitempty"`
	Variant  string   `json:"variant,omitempty"`
	Fields   []string `json:"fields,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Icon     string   `json:"icon,omitempty"`
}

// PlanWarning is a warning that belongs to no one row.
type PlanWarning struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// Ignored is what the input carried that the plan doesn't use.
type Ignored struct {
	Fields   int      `json:"fields"`
	Settings []string `json:"settings"`
	Dropped  []string `json:"dropped"`
}

// ops is the write list behind a plan. It is never serialized.
type ops struct{}

// PlanRestore works out a restore of d onto st in mode merge or replace.
func PlanRestore(st *storage.TransferState, d *Decoded, mode string) *Plan {
	return &Plan{}
}

// PlanImport works out a Homepage import onto st. gatusKeys is the set of
// keys the current Gatus snapshot has (§4.3).
func PlanImport(st *storage.TransferState, hp *Homepage, gatusKeys map[string]bool) *Plan {
	return &Plan{}
}

// Result is the plan as an apply's response: no hash, applied true.
func (p *Plan) Result() *Plan {
	return p
}
