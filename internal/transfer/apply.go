package transfer

import (
	"context"
	"errors"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Writer is what Apply writes through: *storage.TransferTx in production, an
// in-memory state in the pure tests. Every catalog write names the owner.
type Writer interface {
	DeleteCatalog(ctx context.Context, owner string) error
	CreateSection(ctx context.Context, owner, name string, sortIndex, gridWidth int) (string, error)
	UpdateSection(ctx context.Context, owner, id string, sortIndex, gridWidth int) error
	CreateOffer(ctx context.Context, o storage.TransferOfferRow) (string, error)
	UpdateOffer(ctx context.Context, id string, o storage.TransferOfferRow) error
	CreateApp(ctx context.Context, owner string, a storage.TransferAppRow) (string, error)
	UpdateApp(ctx context.Context, owner, id string, a storage.TransferAppRow) error
	SetGatusKey(ctx context.Context, owner, id, key string) error
	LinkLibrary(ctx context.Context, owner string, appIDs []string) error
	PutIcon(ctx context.Context, appID, variant string, png []byte, width, height int, etag string) error
	SetSettings(ctx context.Context, s storage.SystemSettings) error
	CreateKid(ctx context.Context, name, avatarColor string) (string, error)
	SetPerson(ctx context.Context, id string, p storage.TransferPersonRow) error
	AddFavorites(ctx context.Context, userID string, appIDs []string) error
	SetLayout(ctx context.Context, userID string, appIDs []string) error
	SetCollapsed(ctx context.Context, userID string, sectionIDs []string) error
	SetAllowed(ctx context.Context, kidID string, appIDs []string) error
}

// Apply carries out p through w. The caller runs it inside one transaction.
func Apply(ctx context.Context, w Writer, p *Plan) error {
	return errors.New("transfer: apply not implemented")
}
