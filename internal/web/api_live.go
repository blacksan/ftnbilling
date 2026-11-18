package web

// liveRoutes is the hook for the live API routes (docs/release-plan-routers.md
// section 3). h registers a route and, for /api/v1/sites…, its /routers twin:
// register new routes under /api/v1/routers/… only.
func (s *Server) liveRoutes(h func(pattern, role string, f apiFunc)) {}
