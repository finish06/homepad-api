// Package providers holds the widget providers (SPEC-status-widgets §6.5).
// Every provider talks to its app only through the guarded *widgets.Client.
package providers

import (
	"context"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// RED stub — implemented in the GREEN commit.

type stub struct{ kind string }

func (s stub) Kind() string                { return s.kind }
func (s stub) Label() string               { return "" }
func (s stub) Auth() widgets.AuthType      { return widgets.AuthNone }
func (s stub) Fields() []widgets.FieldSpec { return nil }
func (s stub) Match() []string             { return nil }
func (s stub) Help() string                { return "" }
func (s stub) Fetch(context.Context, *widgets.Client, widgets.Config, *widgets.State) (widgets.Result, error) {
	return widgets.Result{}, widgets.ErrUnreachable
}

func Jellyfin() widgets.Provider                     { return stub{"jellyfin"} }
func Sonarr() widgets.Provider                       { return stub{"sonarr"} }
func Radarr() widgets.Provider                       { return stub{"radarr"} }
func sonarrAt(now func() time.Time) widgets.Provider { return stub{"sonarr"} }
func radarrAt(now func() time.Time) widgets.Provider { return stub{"radarr"} }
func Default() *widgets.Registry                     { return widgets.NewRegistry() }
