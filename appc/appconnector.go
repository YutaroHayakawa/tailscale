// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package appc implements App Connectors.
// An AppConnector provides DNS domain oriented routing of traffic. An App
// Connector becomes a DNS server for a peer, authoritative for the set of
// configured domains. DNS resolution of the target domain triggers dynamic
// publication of routes to ensure that traffic to the domain is routed through
// the App Connector.
package appc

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"sync"

	xmaps "golang.org/x/exp/maps"
	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/appc/routeinfo"
	"tailscale.com/ipn"
	"tailscale.com/types/logger"
	"tailscale.com/types/views"
	"tailscale.com/util/dnsname"
	"tailscale.com/util/execqueue"
	"tailscale.com/util/mak"
)

// RouteAdvertiser is an interface that allows the AppConnector to advertise
// newly discovered routes that need to be served through the AppConnector.
type RouteAdvertiser interface {
	AdvertiseRouteInfo(*routeinfo.RouteInfo)

	// Store/ReadRouteInfo persists and retreives RouteInfo to stable storage
	StoreRouteInfo(*routeinfo.RouteInfo) error
	ReadRouteInfo() (*routeinfo.RouteInfo, error)
}

// AppConnector is an implementation of an AppConnector that performs
// its function as a subsystem inside of a tailscale node. At the control plane
// side App Connector routing is configured in terms of domains rather than IP
// addresses.
// The AppConnectors responsibility inside tailscaled is to apply the routing
// and domain configuration as supplied in the map response.
// DNS requests for configured domains are observed. If the domains resolve to
// routes not yet served by the AppConnector the local node configuration is
// updated to advertise the new route.
type AppConnector struct {
	logf            logger.Logf
	routeAdvertiser RouteAdvertiser

	// mu guards the fields that follow
	mu sync.Mutex

	// domains is a map of lower case domain names with no trailing dot, to an
	// ordered list of resolved IP addresses.
	// domains map[string][]netip.Addr

	// controlRoutes is the list of routes that were last supplied by control.
	// controlRoutes []netip.Prefix

	// wildcards is the list of domain strings that match subdomains.
	// wildcards []string

	// the in memory copy of all the routes that's advertised
	routeInfo *routeinfo.RouteInfo

	// queue provides ordering for update operations
	queue execqueue.ExecQueue

	// whether this tailscaled should persist routes. Storing RouteInfo enables the app connector
	// to forget routes when appropriate and should make routes smaller. While we are verifying that
	// writing the RouteInfo to StateStore is a good solution (and doesn't for example cause writes
	// that are too frequent or too large) use a controlknob to manage this flag.
	ShouldStoreRoutes bool
}

// NewAppConnector creates a new AppConnector.
func NewAppConnector(logf logger.Logf, routeAdvertiser RouteAdvertiser, shouldStoreRoutes bool) *AppConnector {
	// TODO(fran) if !shouldStoreRoutes we probably want to try and clean up any stored routes
	return &AppConnector{
		logf:              logger.WithPrefix(logf, "appc: "),
		routeAdvertiser:   routeAdvertiser,
		ShouldStoreRoutes: shouldStoreRoutes,
		routeInfo:         nil,
	}
}

// UpdateDomainsAndRoutes starts an asynchronous update of the configuration
// given the new domains and routes.
func (e *AppConnector) UpdateDomainsAndRoutes(domains []string, routes []netip.Prefix) {
	e.queue.Add(func() {
		// Add the new routes first.
		e.updateRoutes(routes)
		e.updateDomains(domains)
	})
}

func (e *AppConnector) RouteInfo() *routeinfo.RouteInfo {
	if e.routeInfo == nil {
		ret, err := e.routeAdvertiser.ReadRouteInfo()
		if err != nil {
			if err != ipn.ErrStateNotExist {
				e.logf("Unsuccessful Read RouteInfo: ", err)
			}
			return routeinfo.NewRouteInfo()
		}
		return ret
	}
	return e.routeInfo
}

// RecreateRouteInfoFromStore searches from presist store for existing routeInfo for current profile,
// then update local routes of routeInfo in store as the current advertised routes.
func (e *AppConnector) RecreateRouteInfoFromStore(localRoutes []netip.Prefix) {
	e.queue.Add(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		ri := e.RouteInfo()
		err := e.routeAdvertiser.StoreRouteInfo(ri)
		if err != nil {
			e.logf("Appc recreate routeInfo: Error updating routeInfo in store: ", err)
		}
		e.routeAdvertiser.AdvertiseRouteInfo(ri)
		if err != nil {
			e.logf("Appc recreate routeInfo: Error advertise routes: ", err)
		}
		e.routeInfo = ri
	})
}

// UpdateRouteInfo updates routeInfo value of the AppConnector and
// stores the routeInfo value to presist store.
func (e *AppConnector) UpdateRouteInfo(ri *routeinfo.RouteInfo) {
	err := e.routeAdvertiser.StoreRouteInfo(ri)
	if err != nil {
		e.logf("Appc: Failed to remove routeInfo from store: ", err)
	}
	e.routeInfo = ri
}

func (e *AppConnector) UnadvertiseRemoteRoutes() {
	e.queue.Add(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.routeAdvertiser.AdvertiseRouteInfo(nil)
	})
}

// UpdateDomains asynchronously replaces the current set of configured domains
// with the supplied set of domains. Domains must not contain a trailing dot,
// and should be lower case. If the domain contains a leading '*' label it
// matches all subdomains of a domain.
func (e *AppConnector) UpdateDomains(domains []string) {
	e.queue.Add(func() {
		e.updateDomains(domains)
	})
}

// Wait waits for the currently scheduled asynchronous configuration changes to
// complete.
func (e *AppConnector) Wait(ctx context.Context) {
	e.queue.Wait(ctx)
}

func (e *AppConnector) updateDomains(domains []string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var oldDiscovered map[string]*routeinfo.DatedRoutes
	var routeInfo *routeinfo.RouteInfo
	shouldStoreRoutes := e.ShouldStoreRoutes

	routeInfo = e.RouteInfo()
	oldDiscovered, routeInfo.Discovered = routeInfo.Discovered, make(map[string]*routeinfo.DatedRoutes, len(domains))

	routeInfo.Wildcards = routeInfo.Wildcards[:0]
	for _, d := range domains {
		d = strings.ToLower(d)
		if len(d) == 0 {
			continue
		}
		if strings.HasPrefix(d, "*.") {
			routeInfo.Wildcards = append(routeInfo.Wildcards, d[2:])
			continue
		}
		routeInfo.Discovered[d] = oldDiscovered[d]
		delete(oldDiscovered, d)
	}

	for d, dr := range oldDiscovered {
		for _, wc := range routeInfo.Wildcards {
			if dnsname.HasSuffix(d, wc) {
				routeInfo.Discovered[d] = dr
				delete(oldDiscovered, d)
				break
			}
		}
	}

	if shouldStoreRoutes {
		e.UpdateRouteInfo(routeInfo)
	} else {
		e.routeInfo = routeInfo
	}
	e.scheduleAdvertiseRouteInfo(e.RouteInfo())

	e.logf("handling domains: %v and wildcards: %v", xmaps.Keys(e.RouteInfo().Discovered), routeInfo.Wildcards)
}

// updateRoutes merges the supplied routes into the currently configured routes. The routes supplied
// by control for UpdateRoutes are supplemental to the routes discovered by DNS resolution, but are
// also more often whole ranges. UpdateRoutes will remove any single address routes that are now
// covered by new ranges.
func (e *AppConnector) updateRoutes(routes []netip.Prefix) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// If there was no change since the last update, no work to do.
	if slices.Equal(e.RouteInfo().Control, routes) {
		return
	}

	var routeInfo *routeinfo.RouteInfo
	e.routeInfo.Control = routes
	if e.ShouldStoreRoutes {
		routeInfo = e.RouteInfo()
		routeInfo.Control = routes
		e.routeAdvertiser.StoreRouteInfo(e.routeInfo)
	}

	e.routeAdvertiser.AdvertiseRouteInfo(e.routeInfo)
}

// Domains returns the currently configured domain list.
func (e *AppConnector) Domains() views.Slice[string] {
	e.mu.Lock()
	defer e.mu.Unlock()

	return views.SliceOf(xmaps.Keys(e.RouteInfo().Discovered))
}

// DomainRoutes returns a map of domains to resolved IP
// addresses.
func (e *AppConnector) DomainRoutes() map[string][]netip.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.routeInfo.DomainRoutes()
}

// ObserveDNSResponse is a callback invoked by the DNS resolver when a DNS
// response is being returned over the PeerAPI. The response is parsed and
// matched against the configured domains, if matched the routeAdvertiser is
// advised to advertise the discovered route.
func (e *AppConnector) ObserveDNSResponse(res []byte) {
	var p dnsmessage.Parser
	if _, err := p.Start(res); err != nil {
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		return
	}

	// cnameChain tracks a chain of CNAMEs for a given query in order to reverse
	// a CNAME chain back to the original query for flattening. The keys are
	// CNAME record targets, and the value is the name the record answers, so
	// for www.example.com CNAME example.com, the map would contain
	// ["example.com"] = "www.example.com".
	var cnameChain map[string]string

	// addressRecords is a list of address records found in the response.
	var addressRecords map[string][]netip.Addr

	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return
		}

		if h.Class != dnsmessage.ClassINET {
			if err := p.SkipAnswer(); err != nil {
				return
			}
			continue
		}

		switch h.Type {
		case dnsmessage.TypeCNAME, dnsmessage.TypeA, dnsmessage.TypeAAAA:
		default:
			if err := p.SkipAnswer(); err != nil {
				return
			}
			continue

		}

		domain := strings.TrimSuffix(strings.ToLower(h.Name.String()), ".")
		if len(domain) == 0 {
			continue
		}

		if h.Type == dnsmessage.TypeCNAME {
			res, err := p.CNAMEResource()
			if err != nil {
				return
			}
			cname := strings.TrimSuffix(strings.ToLower(res.CNAME.String()), ".")
			if len(cname) == 0 {
				continue
			}
			mak.Set(&cnameChain, cname, domain)
			continue
		}

		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return
			}
			addr := netip.AddrFrom4(r.A)
			mak.Set(&addressRecords, domain, append(addressRecords[domain], addr))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return
			}
			addr := netip.AddrFrom16(r.AAAA)
			mak.Set(&addressRecords, domain, append(addressRecords[domain], addr))
		default:
			if err := p.SkipAnswer(); err != nil {
				return
			}
			continue
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	routeInfo := e.RouteInfo()
	for domain, addrs := range addressRecords {
		domain, isRouted := e.findRoutedDomainLocked(domain, cnameChain)

		// domain and none of the CNAMEs in the chain are routed
		if !isRouted {
			continue
		}

		// advertise each address we have learned for the routed domain, that
		// was not already known.
		var domainPrefixs []netip.Prefix
		for _, addr := range addrs {
			domainPrefixs = append(domainPrefixs, netip.PrefixFrom(addr, addr.BitLen()))
		}

		e.logf("[v2] observed new routes for %s: %s", domain, domainPrefixs)
		routeInfo.AddRoutesInDiscoveredForDomain(domain, domainPrefixs)
		if e.ShouldStoreRoutes {
			e.UpdateRouteInfo(routeInfo)
		}
	}
	e.scheduleAdvertiseRouteInfo(e.RouteInfo())
}

// starting from the given domain that resolved to an address, find it, or any
// of the domains in the CNAME chain toward resolving it, that are routed
// domains, returning the routed domain name and a bool indicating whether a
// routed domain was found.
// e.mu must be held.
func (e *AppConnector) findRoutedDomainLocked(domain string, cnameChain map[string]string) (string, bool) {
	var isRouted bool
	for {
		_, isRouted = e.RouteInfo().Discovered[domain]
		if isRouted {
			break
		}

		// match wildcard domains
		for _, wc := range e.RouteInfo().Wildcards {
			if dnsname.HasSuffix(domain, wc) {
				e.routeInfo.Discovered[domain] = nil
				isRouted = true
				break
			}
		}

		next, ok := cnameChain[domain]
		if !ok {
			break
		}
		domain = next
	}
	return domain, isRouted
}

func (e *AppConnector) scheduleAdvertiseRouteInfo(ri *routeinfo.RouteInfo) {
	e.queue.Add(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.routeAdvertiser.AdvertiseRouteInfo(ri)
	})
}
