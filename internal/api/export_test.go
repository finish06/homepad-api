package api

import (
	"context"
	"errors"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// RouteInfo is one registered route and its kid class, for the route-coverage
// test (routes_test.go).
type RouteInfo struct {
	Pattern string
	Class   string
}

// RegisteredRoutes builds the handler exactly as New does and returns what the
// router registered — the real registration, not a copy.
func RegisteredRoutes(d Deps) []RouteInfo {
	s := newServer(d)
	_, routes := s.routes(d)
	out := make([]RouteInfo, 0, len(routes))
	for _, r := range routes {
		out = append(out, RouteInfo{Pattern: r.pattern, Class: string(r.class)})
	}
	return out
}

// NewWithTransferFault builds the handler as New does, but every restore or
// import apply writes through a Writer that fails on its failAt-th write. It
// proves an apply is all-or-nothing (SPEC-backup-import AC-009).
func NewWithTransferFault(d Deps, failAt int) http.Handler {
	s := newServer(d)
	s.transferWrap = func(w transfer.Writer) transfer.Writer {
		return &faultWriter{Writer: w, left: failAt}
	}
	h, _ := s.routes(d)
	return h
}

var errInjected = errors.New("injected transfer fault")

// faultWriter passes writes through until left reaches zero, then fails.
type faultWriter struct {
	transfer.Writer
	left int
}

func (f *faultWriter) step() error {
	f.left--
	if f.left <= 0 {
		return errInjected
	}
	return nil
}

func (f *faultWriter) DeleteCatalog(ctx context.Context, owner string) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.DeleteCatalog(ctx, owner)
}

func (f *faultWriter) CreateSection(ctx context.Context, owner, name string, sortIndex, width int) (string, error) {
	if err := f.step(); err != nil {
		return "", err
	}
	return f.Writer.CreateSection(ctx, owner, name, sortIndex, width)
}

func (f *faultWriter) UpdateSection(ctx context.Context, owner, id string, sortIndex, width int) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.UpdateSection(ctx, owner, id, sortIndex, width)
}

func (f *faultWriter) CreateOffer(ctx context.Context, o storage.TransferOfferRow) (string, error) {
	if err := f.step(); err != nil {
		return "", err
	}
	return f.Writer.CreateOffer(ctx, o)
}

func (f *faultWriter) UpdateOffer(ctx context.Context, id string, o storage.TransferOfferRow) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.UpdateOffer(ctx, id, o)
}

func (f *faultWriter) CreateApp(ctx context.Context, owner string, a storage.TransferAppRow) (string, error) {
	if err := f.step(); err != nil {
		return "", err
	}
	return f.Writer.CreateApp(ctx, owner, a)
}

func (f *faultWriter) UpdateApp(ctx context.Context, owner, id string, a storage.TransferAppRow) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.UpdateApp(ctx, owner, id, a)
}

func (f *faultWriter) PutIcon(ctx context.Context, appID, variant string, png []byte, w, h int, etag string) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.PutIcon(ctx, appID, variant, png, w, h, etag)
}

func (f *faultWriter) CreateKid(ctx context.Context, name, color string) (string, error) {
	if err := f.step(); err != nil {
		return "", err
	}
	return f.Writer.CreateKid(ctx, name, color)
}

func (f *faultWriter) SetPerson(ctx context.Context, id string, p storage.TransferPersonRow) error {
	if err := f.step(); err != nil {
		return err
	}
	return f.Writer.SetPerson(ctx, id, p)
}
