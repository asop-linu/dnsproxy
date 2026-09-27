package proxy

import (
	"net"
	"slices"

	"github.com/AdguardTeam/golibs/netutil"
	"github.com/miekg/dns"
)

// cacheForContext returns cache object for the given context.
func (p *Proxy) cacheForContext(d *DNSContext) (c *cache) {
	if d.CustomUpstreamConfig != nil && d.CustomUpstreamConfig.cache != nil {
		return d.CustomUpstreamConfig.cache
	}

	return p.cache
}

// replyFromCache tries to get the response from general or subnet cache.  In
// case the cache is present in d, it's used first.  Returns true on success.
func (p *Proxy) replyFromCache(d *DNSContext) (hit bool) {
	dctxCache := p.cacheForContext(d)

	var ci *cacheItem
	var cacheSource string
	var expired bool
	var key []byte

	// TODO(d.kolyshev): Use EnableEDNSClientSubnet from dctxCache.
	if p.enableEDNSClientSubnet && d.ReqECS != nil {
		ci, expired, key = dctxCache.getWithSubnet(d.Req, d.ReqECS)
		cacheSource = "subnet cache"
	} else {
		ci, expired, key = dctxCache.get(d.Req)
		cacheSource = "general cache"
	}

	if hit = ci != nil; !hit {
		return hit
	}

	d.Res = ci.m
	d.queryStatistics = cachedQueryStatistics(ci.u)

	p.logger.Debug(
		"replying from cache",
		"source", cacheSource,
		"ecs_enabled", p.enableEDNSClientSubnet,
	)

	if dctxCache.optimistic && expired {
		// Build a reduced clone of the current context to avoid data race.
		minCtxClone := &DNSContext{
			// It is only read inside the optimistic resolver.
			CustomUpstreamConfig: d.CustomUpstreamConfig,
			ReqECS:               cloneIPNet(d.ReqECS),
			IsPrivateClient:      d.IsPrivateClient,
		}
		if d.Req != nil {
			minCtxClone.Req = d.Req.Copy()
		}

		go p.shortFlighter.resolveOnce(minCtxClone, key, p.logger)
	}

	return hit
}

// cloneIPNet returns a deep clone of n.
func cloneIPNet(n *net.IPNet) (clone *net.IPNet) {
	if n == nil {
		return nil
	}

	return &net.IPNet{
		IP:   slices.Clone(n.IP),
		Mask: slices.Clone(n.Mask),
	}
}

// setCachedECS adds an EDNS Client Subnet option to the response in d, if the
// request contained a subnet and the response doesn't already have one.
//
// This is used for the responses served from the subnet cache, since
// [cacheResp] stores them without the option whenever the upstream didn't echo
// it back.  Without it, a client would have no way of telling that the answer
// has been cached for its own subnet.  Both SOURCE PREFIX-LENGTH and SCOPE
// PREFIX-LENGTH are set to the requested subnet length, which marks the answer
// as valid for that subnet only.
//
// See RFC 7871 Section 7.3.1.
func setCachedECS(d *DNSContext) {
	if d.Res == nil || d.ReqECS == nil || !d.clientHadEDNS0 {
		// The response must not contain an OPT record if the client didn't use
		// EDNS0, so there is nowhere to put the EDNS Client Subnet option.
		//
		// See https://tools.ietf.org/html/rfc6891.
		return
	}

	ones, _ := d.ReqECS.Mask.Size()
	if ones == 0 {
		// Don't add a meaningless /0 option, it would claim that the answer is
		// valid for every address.
		return
	}

	e, ok := ecsSubnetOption(d.ReqECS, ones)
	if !ok {
		return
	}

	// A server must not add an option that is already present, and the one from
	// the upstream is more authoritative, so leave the response as is.
	if opt := d.Res.IsEdns0(); opt != nil {
		if hasECSSubnet(opt) {
			return
		}

		opt.Option = append(opt.Option, e)

		return
	}

	// Create an OPT record with the client's advertised UDP size, since
	// [DNSContext.scrub] won't add one for a message that already has it.
	o := &dns.OPT{
		Hdr: dns.RR_Header{
			Name:   ".",
			Rrtype: dns.TypeOPT,
		},
		Option: []dns.EDNS0{e},
	}
	o.SetUDPSize(d.udpSize)
	d.Res.Extra = append(d.Res.Extra, o)
}

// ecsSubnetOption returns an EDNS Client Subnet option for the given subnet.
//
//	The address part is masked, as required by RFC 7871 Section 6.
func ecsSubnetOption(n *net.IPNet, ones int) (e *dns.EDNS0_SUBNET, ok bool) {
	// Use the subnet address, not the address of the client.
	if ip4 := n.IP.To4(); ip4 != nil {
		return &dns.EDNS0_SUBNET{
			Code:          dns.EDNS0SUBNET,
			Family:        1,
			SourceNetmask: uint8(ones),
			SourceScope:   uint8(ones),
			Address:       ip4.Mask(net.CIDRMask(ones, netutil.IPv4BitLen)),
		}, true
	}

	if ip16 := n.IP.To16(); ip16 != nil {
		return &dns.EDNS0_SUBNET{
			Code:          dns.EDNS0SUBNET,
			Family:        2,
			SourceNetmask: uint8(ones),
			SourceScope:   uint8(ones),
			Address:       ip16.Mask(net.CIDRMask(ones, netutil.IPv6BitLen)),
		}, true
	}

	return nil, false
}

// hasECSSubnet reports whether opt contains an EDNS Client Subnet option.
func hasECSSubnet(opt *dns.OPT) (ok bool) {
	for _, o := range opt.Option {
		if o.Option() == dns.EDNS0SUBNET {
			return true
		}
	}

	return false
}

// cacheResp stores the response from d in general or subnet cache.  In case the
// cache is present in d, it's used first.
func (p *Proxy) cacheResp(d *DNSContext) {
	dctxCache := p.cacheForContext(d)

	if !p.enableEDNSClientSubnet {
		dctxCache.set(d.Req, d.Res, d.Upstream, p.logger)

		return
	}

	switch ecs, scope := ecsFromMsg(d.Res); {
	case ecs != nil && d.ReqECS != nil:
		ones, bits := ecs.Mask.Size()
		reqOnes, _ := d.ReqECS.Mask.Size()

		// If FAMILY, SOURCE PREFIX-LENGTH, and SOURCE PREFIX-LENGTH bits of
		// ADDRESS in the response don't match the non-zero fields in the
		// corresponding query, the full response MUST be dropped.
		//
		// See RFC 7871 Section 7.3.
		//
		// TODO(a.meshkov):  The whole response MUST be dropped if ECS in it
		// doesn't correspond.
		if !ecs.IP.Mask(ecs.Mask).Equal(d.ReqECS.IP.Mask(d.ReqECS.Mask)) || ones != reqOnes {
			p.logger.Debug(
				"not caching response; subnet mismatch",
				"ecs", ecs,
				"req_ecs", d.ReqECS,
			)

			return
		}

		// If SCOPE PREFIX-LENGTH is longer than SOURCE PREFIX-LENGTH, store
		// SCOPE PREFIX-LENGTH bits of ADDRESS, and then mark the response as
		// valid for all addresses that fall within that range.
		//
		// See RFC 7871 Section 7.3.1.
		//
		// A zero SCOPE PREFIX-LENGTH is not an indication that the response is
		// valid for every subnet.  It only means that the server didn't
		// include the option into the response at all, or deliberately left
		// the scope empty.  The answer has still been computed for the subnet
		// from the request, so it must only be stored for that subnet.  See the
		// d.ReqECS case below.
		if scope > 0 && scope < reqOnes {
			ecs.Mask = net.CIDRMask(scope, bits)
			ecs.IP = ecs.IP.Mask(ecs.Mask)
		}

		p.logger.Debug("caching response", "ecs", ecs)

		dctxCache.setWithSubnet(d.Req, d.Res, d.Upstream, ecs, p.logger)
	case d.ReqECS != nil:
		// The response contains no EDNS Client Subnet option, or its scope is
		// zero.  That doesn't make the answer global: the upstream has still
		// resolved it for the subnet sent in the request, it just didn't echo
		// the option back.  Storing it under a null subnet would make every
		// other subnet match this single entry, since the longest-prefix-match
		// lookup in [cache.getWithSubnet] always terminates at mask zero.
		//
		// Store the response under the requested subnet instead, so that each
		// client subnet gets its own entry.  See RFC 7871 Section 7.3.1.
		p.logger.Debug("caching response for requested subnet", "ecs", d.ReqECS)

		dctxCache.setWithSubnet(d.Req, d.Res, d.Upstream, d.ReqECS, p.logger)
	default:
		dctxCache.set(d.Req, d.Res, d.Upstream, p.logger)
	}
}

// ClearCache clears the DNS cache of p.
func (p *Proxy) ClearCache() {
	if p.cache == nil {
		return
	}

	p.cache.clearItems()
	p.cache.clearItemsWithSubnet()
	p.logger.Debug("cache cleared")
}
