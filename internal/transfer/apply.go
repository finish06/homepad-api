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

// ErrNoOps is returned by Apply for a plan that didn't come from a planner.
var ErrNoOps = errors.New("transfer: plan has no operations")

// Apply carries out p through w, in dependency order: Replace's delete,
// sections, offers, apps, provenance links, icons, settings, status keys,
// people, then the caller's order. The caller runs it inside one
// transaction, so a failure anywhere leaves nothing behind (AC-009).
func Apply(ctx context.Context, w Writer, p *Plan) error {
	o := p.ops
	if o == nil {
		return ErrNoOps
	}
	secID := map[string]string{}
	appID := map[string]string{}
	if !o.replace {
		for k, v := range o.sectionIDs {
			secID[k] = v
		}
		for k, v := range o.appIDs {
			appID[k] = v
		}
	}
	if o.replace {
		if err := w.DeleteCatalog(ctx, o.owner); err != nil {
			return err
		}
	}
	for _, s := range o.sections {
		if s.id == "" {
			id, err := w.CreateSection(ctx, o.owner, s.name, s.sortIndex, s.gridWidth)
			if err != nil {
				return err
			}
			secID[s.name] = id
			continue
		}
		if err := w.UpdateSection(ctx, o.owner, s.id, s.sortIndex, s.gridWidth); err != nil {
			return err
		}
	}
	for _, of := range o.offers {
		if of.id == "" {
			if _, err := w.CreateOffer(ctx, of.row); err != nil {
				return err
			}
			continue
		}
		if err := w.UpdateOffer(ctx, of.id, of.row); err != nil {
			return err
		}
	}
	for _, a := range o.apps {
		row := a.row
		if a.section != nil {
			if id, ok := secID[*a.section]; ok {
				row.SectionID = &id
			}
		}
		if a.id == "" {
			id, err := w.CreateApp(ctx, o.owner, row)
			if err != nil {
				return err
			}
			appID[row.Slug] = id
			continue
		}
		if err := w.UpdateApp(ctx, o.owner, a.id, row); err != nil {
			return err
		}
	}
	if len(o.link) > 0 {
		ids := make([]string, 0, len(o.link))
		for _, slug := range o.link {
			ids = append(ids, appID[slug])
		}
		if err := w.LinkLibrary(ctx, o.owner, ids); err != nil {
			return err
		}
	}
	for _, ic := range o.icons {
		if err := w.PutIcon(ctx, appID[ic.slug], ic.variant, ic.png, ic.width, ic.height, ic.etag); err != nil {
			return err
		}
	}
	if o.settings != nil {
		if err := w.SetSettings(ctx, *o.settings); err != nil {
			return err
		}
	}
	for _, g := range o.gatus {
		if err := w.SetGatusKey(ctx, o.owner, g.id, g.key); err != nil {
			return err
		}
	}
	ids := func(slugs []string) []string {
		out := make([]string, 0, len(slugs))
		for _, s := range slugs {
			if id, ok := appID[s]; ok {
				out = append(out, id)
			}
		}
		return out
	}
	for _, pr := range o.people {
		id := pr.id
		if id == "" {
			var err error
			if id, err = w.CreateKid(ctx, pr.kidName, pr.kidColor); err != nil {
				return err
			}
		}
		if pr.set != nil {
			if err := w.SetPerson(ctx, id, *pr.set); err != nil {
				return err
			}
		}
		if pr.allowed != nil {
			if err := w.SetAllowed(ctx, id, ids(*pr.allowed)); err != nil {
				return err
			}
		}
		if len(pr.addFavs) > 0 {
			if err := w.AddFavorites(ctx, id, ids(pr.addFavs)); err != nil {
				return err
			}
		}
		if pr.order != nil {
			if err := w.SetLayout(ctx, id, ids(*pr.order)); err != nil {
				return err
			}
		}
		if pr.collapsed != nil {
			sids := make([]string, 0, len(*pr.collapsed))
			for _, n := range *pr.collapsed {
				if sid, ok := secID[n]; ok {
					sids = append(sids, sid)
				}
			}
			if err := w.SetCollapsed(ctx, id, sids); err != nil {
				return err
			}
		}
	}
	if o.callerLayout != nil {
		order := make([]string, 0, len(o.callerLayout))
		for _, r := range o.callerLayout {
			if r.id != "" {
				order = append(order, r.id)
			} else if id, ok := appID[r.slug]; ok {
				order = append(order, id)
			}
		}
		if err := w.SetLayout(ctx, o.caller, order); err != nil {
			return err
		}
	}
	return nil
}
