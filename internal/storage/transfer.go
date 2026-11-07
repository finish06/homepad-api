package storage

import (
	"context"
	"errors"
)

// SPEC-backup-import §7 — the storage side of backup, restore and Homepage
// import. The planner (internal/transfer) is pure: it reads a TransferState and
// writes through a TransferTx. Every catalog row it reads or writes is the
// shared catalog owner's (AC-025, homepad-api#87), whichever admin runs it.

// importLockKey is the advisory lock that serializes restore and import
// applies (AC-009). It is distinct from migrateLockKey.
const importLockKey = 0x686f6d6570616432 // "homepad2"

var errTransferNotImplemented = errors.New("storage: transfer not implemented")

// TransferState is everything the planner needs to know about the install.
type TransferState struct {
	// OwnerID is the shared catalog owner (the first admin).
	OwnerID string
	// CallerID is the admin running the export, restore or import.
	CallerID string
	Sections []TransferSection // in sort_index order
	Apps     []TransferApp     // the owner's services, by slug
	Offers   []TransferOffer   // in sort_index order
	Settings SystemSettings
	// People is every account, adults then kids. Loaded only when asked for.
	People []TransferPerson
	// CallerOrder is the caller's current view order of the catalog (the
	// ListServices order: saved layout first, then name). Import appends to it.
	CallerOrder []string
	// OtherAccounts counts accounts other than the caller.
	OtherAccounts int
}

// TransferSection is one category of the shared catalog.
type TransferSection struct {
	ID        string
	Name      string
	SortIndex int
	GridWidth int
}

// TransferApp is one service of the shared catalog.
type TransferApp struct {
	ID              string
	Slug            string
	Name            string
	Description     string
	URL             string
	Icon            string
	GatusKey        string
	ClickAction     string
	SectionID       *string
	SourceLibraryID *string
	Icons           []TransferIcon // light before dark
}

// TransferIcon is one uploaded icon variant. Bytes is filled only when the
// state was loaded for an export.
type TransferIcon struct {
	Variant string
	ETag    string
	Bytes   []byte
}

// TransferOffer is one App Library offer.
type TransferOffer struct {
	ID                string
	Name              string
	URL               string
	Icon              string
	Description       string
	SuggestedCategory string
	SortIndex         int
}

// TransferPerson is one account with its per-person dashboard state. Only the
// fields a backup may carry are here: no password hash, no session.
type TransferPerson struct {
	ID                string
	Role              string // admin | user | kid
	Email             string // "" for a kid
	DisplayName       string // "" when unset (adults); the name (kids)
	AvatarColor       string // "" when unset
	ThemePref         string
	DensityPref       string
	ShowHealthBar     bool
	ShowUptimeDisplay bool
	Favorites         []string // service ids in the shared catalog
	Order             []string // service ids, saved layout order
	Collapsed         []string // category ids
	Allowed           []string // service ids (kids)
}

// TransferLoad says what LoadTransferState reads.
type TransferLoad struct {
	OwnerID   string
	CallerID  string
	People    bool // per-person state for every account
	IconBytes bool // icon bytes (export only)
}

// TransferAppRow is the full desired state of an app on create or update.
type TransferAppRow struct {
	Slug        string
	Name        string
	Description string
	URL         string
	Icon        string
	GatusKey    string // "" = unmonitored (NULL)
	ClickAction string
	SectionID   *string
}

// TransferOfferRow is the desired state of an App Library offer.
type TransferOfferRow struct {
	Name              string
	URL               string
	Icon              string
	Description       string
	SuggestedCategory string
	SortIndex         int
}

// TransferPersonRow is the per-person fields a restore sets.
type TransferPersonRow struct {
	DisplayName       *string // nil = NULL (adults only; a kid keeps its name)
	AvatarColor       *string // nil = NULL
	ThemePref         string
	DensityPref       string
	ShowHealthBar     bool
	ShowUptimeDisplay bool
}

// LoadTransferState reads the state for a preview or an export.
func (s *Store) LoadTransferState(ctx context.Context, in TransferLoad) (*TransferState, error) {
	return nil, errTransferNotImplemented
}

// TransferTx is one apply: a transaction holding importLockKey.
type TransferTx struct {
	store *Store
}

// InTransferTx runs fn in one transaction holding the import advisory lock.
// fn's error rolls everything back and is returned as is.
func (s *Store) InTransferTx(ctx context.Context, fn func(tx *TransferTx) error) error {
	return errTransferNotImplemented
}
