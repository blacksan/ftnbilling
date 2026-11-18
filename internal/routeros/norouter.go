package routeros

import (
	"context"
	"errors"
)

// ErrNoRouter is returned by every call on the client given out for a site
// that does not exist, is not joined yet, or is paused.
var ErrNoRouter = errors.New("no router is available for this building")

type noRouter struct{}

// NoRouter returns a Client whose every call fails with ErrNoRouter.
func NoRouter() Client { return noRouter{} }

func (noRouter) Ping(context.Context) error                   { return ErrNoRouter }
func (noRouter) EnsureProfile(context.Context, Profile) error { return ErrNoRouter }
func (noRouter) EnsureUser(context.Context, User) error       { return ErrNoRouter }
func (noRouter) RemoveUser(context.Context, string) error     { return ErrNoRouter }
func (noRouter) RemoveProfile(context.Context, string) error  { return ErrNoRouter }
func (noRouter) KickUser(context.Context, string) error       { return ErrNoRouter }
func (noRouter) Login(context.Context, string, string, string, string) error {
	return ErrNoRouter
}
func (noRouter) Hosts(context.Context) ([]Host, error)               { return nil, ErrNoRouter }
func (noRouter) ActiveSessions(context.Context) ([]Session, error)   { return nil, ErrNoRouter }
func (noRouter) SimpleQueues(context.Context) ([]SimpleQueue, error) { return nil, ErrNoRouter }
func (noRouter) Interfaces(context.Context) ([]Iface, error)         { return nil, ErrNoRouter }
func (noRouter) DNSCache(context.Context) ([]DNSEntry, error)        { return nil, ErrNoRouter }
func (noRouter) PingHost(context.Context, string, int) (PingResult, error) {
	return PingResult{}, ErrNoRouter
}
func (noRouter) System(context.Context) (SysInfo, error) { return SysInfo{}, ErrNoRouter }

func (noRouter) Execute(context.Context, string) (string, error) { return "", ErrNoRouter }
func (noRouter) Traceroute(context.Context, string, int) ([]Hop, error) {
	return nil, ErrNoRouter
}
func (noRouter) Leases(context.Context, string) ([]Lease, error) { return nil, ErrNoRouter }
