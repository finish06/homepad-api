package api

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
