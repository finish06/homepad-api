// Package transfer is the backup, restore and Homepage import engine
// (SPEC-backup-import). It is pure: it reads a storage.TransferState, works out
// a Plan, and applies a Plan through a Writer. Preview and apply run the same
// Plan function, so what an admin previews is what lands (§7).
package transfer

import "net/http"

// Error is a refusal the API turns into a JSON error body. Code is a stable
// code the UI owns the words for (spec §8, UX §6); Message is a plain English
// fallback. Neither ever carries file contents.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Count   int    `json:"count,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Things  string `json:"things,omitempty"`
	// Detail is the YAML parser's own words for a yaml_syntax error ("did
	// not find expected key"), for the UI's "There's a problem on line {n}:
	// {detail}." It never quotes the file.
	Detail string `json:"detail,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Error codes.
const (
	CodeNotABackup          = "not_a_backup"
	CodeSchemaNewer         = "schema_newer"
	CodeInvalidJSON         = "invalid_json"
	CodeNotHomepageServices = "not_homepage_services"
	CodeNotHomepageBookmark = "not_homepage_bookmarks"
	CodeNotHomepageSettings = "not_homepage_settings"
	CodeYAMLSyntax          = "yaml_syntax"
	CodeTooDeep             = "too_deep"
	CodeTooLarge            = "too_large"
	CodeTooMany             = "too_many"
	CodePlanChanged         = "plan_changed"
	CodeBadMode             = "bad_mode"
	CodePlanMissing         = "plan_missing"
)

// Skip reasons.
const (
	ReasonNoLink         = "no_link"
	ReasonBadScheme      = "bad_scheme"
	ReasonHrefVariable   = "href_uses_variable"
	ReasonAlreadyHere    = "already_here"
	ReasonDuplicate      = "duplicate"
	ReasonNoName         = "no_name"
	ReasonInvalidItem    = "invalid_item"
	ReasonAdultNoAccount = "adult_no_account"
	ReasonIconInvalid    = "icon_invalid"
)

// Warnings.
const (
	WarnWidgetDropped       = "widget_dropped"
	WarnIconLocal           = "icon_local"
	WarnIconGlyph           = "icon_glyph"
	WarnIconUnsupported     = "icon_unsupported"
	WarnIconInvalid         = "icon_invalid"
	WarnDescriptionVariable = "description_variable"
	WarnSectionMissing      = "section_missing"
	WarnKidNeedsDevice      = "kid_needs_device"
	WarnStatusOn            = "status_on"
	WarnRefMissing          = "ref_missing"
	WarnUnknownFields       = "unknown_fields"
)

func errorf(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// tooMany is the 422 for a count over its limit (AC-026).
func tooMany(things string, count, limit int) *Error {
	return &Error{
		Status: http.StatusUnprocessableEntity, Code: CodeTooMany,
		Message: "too many " + things, Count: count, Limit: limit, Things: things,
	}
}
