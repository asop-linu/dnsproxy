package proxy

import (
	"cmp"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AdguardTeam/golibs/netutil"
	"github.com/AdguardTeam/golibs/testutil"
	"github.com/AdguardTeam/golibs/testutil/servicetest"
	"github.com/asop-linu/dnsproxy/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/miekg/dns"
)

// testCacheSize is the maximum size of cache for tests.
const testCacheSize = 4096

// testUpsAddr is the mock upstream address for tests.
const testUpsAddr = "https://upstream.address"

// upstreamWithAddr is a [dnsproxytest.Upstream] that is only expected to be
// used to get its address.
var upstreamWithAddr = &testUpstream{
	OnExchange: func(m *dns.Msg) (_ *dns.Msg, _ error) { panic(testutil.UnexpectedCall(m)) },
	OnClose:    func() (_ error) { panic(testutil.UnexpectedCall()) },
	OnAddress:  func() (addr string) { return testUpsAddr },
}

// newTestCache is a helper that returns new cache and fills its config with
// given values.  If conf is nil, the default configuration will be used.
func newTestCache(tb testing.TB, conf *cacheConfig) (c *cache) {
	tb.Helper()

	conf = cmp.Or(conf, &cacheConfig{})

	return newCache(&cacheConfig{
		size:             cmp.Or(conf.size, testCacheSize),
		optimisticTTL:    cmp.Or(conf.optimisticTTL, testOptimisticTTL),
		optimisticMaxAge: cmp.Or(conf.optimisticMaxAge, testOptimisticMaxAge),
		withECS:          conf.withECS,
		optimistic:       conf.optimistic,
	})
}

func TestServeCached(t *testing.T) {
	dnsProxy := mustNew(t, &Config{
		Logger:         testLogger,
		UDPListenAddr:  []*net.UDPAddr{net.UDPAddrFromAddrPort(localhostAnyPort)},
		TCPListenAddr:  []*net.TCPAddr{net.TCPAddrFromAddrPort(localhostAnyPort)},
		UpstreamConfig: newTestUpstreamConfig(t, defaultTimeout, testDefaultUpstreamAddr),
		TrustedProxies: defaultTrustedProxies,
		DNSSECEnabled:  false,
		CacheEnabled:   true,
	})

	// Start listening.
	servicetest.RequireRun(t, dnsProxy, testTimeout)

	// Create a DNS request.
	request := (&dns.Msg{}).SetQuestion("google.com.", dns.TypeA)
	request.SetEdns0(defaultUDPBufSize, false)

	// Fill the cache.
	reply := (&dns.Msg{
		Answer: []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
	}).SetReply(request)
	reply.SetEdns0(defaultUDPBufSize, false)

	dnsProxy.cache.set(request, reply, upstreamWithAddr, testLogger)

	// Create a DNS-over-UDP client connection.
	addr := dnsProxy.Addr(ProtoUDP)
	client := &dns.Client{
		Net:     string(ProtoUDP),
		Timeout: testTimeout,
	}

	r, _, err := client.Exchange(request, addr.String())
	require.NoErrorf(t, err, "error in the first request: %s", err)

	requireEqualMsgs(t, r, reply)
}

func TestCache_expired(t *testing.T) {
	const host = "google.com."

	ans := &dns.A{
		Hdr: dns.RR_Header{
			Name:   host,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
		},
		A: net.IP{8, 8, 8, 8},
	}
	reply := (&dns.Msg{
		MsgHdr: dns.MsgHdr{
			Response: true,
		},
		Answer: []dns.RR{ans},
	}).SetQuestion(host, dns.TypeA)

	testCases := []struct {
		name             string
		ttl              uint32
		wantTTL          uint32
		optimistic       bool
		optimisticMaxAge time.Duration
	}{{
		name: "realistic_hit",

		ttl:        defaultTestTTL,
		wantTTL:    defaultTestTTL,
		optimistic: false,
	}, {
		name:       "realistic_miss",
		ttl:        0,
		wantTTL:    0,
		optimistic: false,
	}, {
		name:             "optimistic_hit",
		ttl:              defaultTestTTL,
		wantTTL:          defaultTestTTL,
		optimistic:       true,
		optimisticMaxAge: testOptimisticMaxAge,
	}, {
		name:             "optimistic_expired",
		ttl:              0,
		wantTTL:          uint32(testOptimisticTTL.Seconds()),
		optimistic:       true,
		optimisticMaxAge: testOptimisticMaxAge,
	}, {
		name:             "optimistic_max_age_exceeded",
		ttl:              0,
		wantTTL:          0,
		optimistic:       true,
		optimisticMaxAge: time.Hour * -1,
	}}

	testCache := newTestCache(t, nil)
	for _, tc := range testCases {
		ans.Hdr.Ttl = tc.ttl
		req := (&dns.Msg{}).SetQuestion(host, dns.TypeA)

		t.Run(tc.name, func(t *testing.T) {
			if tc.optimistic {
				testCache.optimistic = true
				testCache.optimisticMaxAge = tc.optimisticMaxAge
				t.Cleanup(func() { testCache.optimistic = false })
			}

			key := msgToKey(reply)
			data := (&cacheItem{
				m:   reply,
				u:   testUpsAddr,
				ttl: tc.ttl,
			}).pack()
			shardIdx := int(hashKey(key) % numShards)
			testCache.setGlcCacheItem(shardIdx, key, data)
			t.Cleanup(testCache.clearItems)

			r, expired, key := testCache.get(req)
			assert.Equal(t, msgToKey(req), key)
			assert.Equal(t, tc.ttl == 0, expired)

			if tc.wantTTL != 0 {
				require.NotNil(t, r)

				assert.Equal(t, tc.wantTTL, r.m.Answer[0].Header().Ttl)
				assert.Equal(t, testUpsAddr, r.u)
			} else {
				require.Nil(t, r)
			}
		})
	}
}

func TestCacheDO(t *testing.T) {
	testCache := newTestCache(t, nil)

	// Make a request.
	request := (&dns.Msg{}).SetQuestion("google.com.", dns.TypeA)

	// Fill the cache.
	reply := (&dns.Msg{
		MsgHdr: dns.MsgHdr{
			Response: true,
		},
		Answer: []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
	}).SetQuestion("google.com.", dns.TypeA)
	reply.SetEdns0(4096, false)

	// Store in cache.
	testCache.set(request, reply, upstreamWithAddr, testLogger)

	t.Run("without_do", func(t *testing.T) {
		ci, expired, _ := testCache.get(request)
		assert.False(t, expired)
		assert.NotNil(t, ci)
	})

	t.Run("with_do", func(t *testing.T) {
		reqClone := request.Copy()
		t.Cleanup(func() {
			request = reqClone
		})

		request.SetEdns0(4096, true)

		ci, expired, _ := testCache.get(request)
		require.Nil(t, ci)
		assert.False(t, expired)
	})
}

func TestCacheCNAME(t *testing.T) {
	testCache := newTestCache(t, nil)

	// Create a DNS request.
	request := (&dns.Msg{}).SetQuestion("google.com.", dns.TypeA)

	// Fill the cache
	reply := (&dns.Msg{
		MsgHdr: dns.MsgHdr{
			Response: true,
		},
		Answer: []dns.RR{newRR(t, "google.com.", dns.TypeCNAME, 3600, "test.google.com.")},
	}).SetQuestion("google.com.", dns.TypeA)
	testCache.set(request, reply, upstreamWithAddr, testLogger)

	t.Run("no_cnames", func(t *testing.T) {
		r, expired, _ := testCache.get(request)
		assert.Nil(t, r)
		assert.False(t, expired)
	})

	// Now fill the cache with a cacheable CNAME response.
	reply.Answer = append(reply.Answer, newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8}))
	testCache.set(request, reply, upstreamWithAddr, testLogger)

	// We are testing that a proper CNAME response gets cached
	t.Run("cnames_exist", func(t *testing.T) {
		r, expired, key := testCache.get(request)
		assert.False(t, expired)
		assert.Equal(t, key, msgToKey(request))

		require.NotNil(t, r)

		assert.Equal(t, testUpsAddr, r.u)
	})
}

func TestCache_uncacheable(t *testing.T) {
	testCache := newTestCache(t, nil)

	// Create a DNS request.
	request := (&dns.Msg{}).SetQuestion("google.com.", dns.TypeA)
	// Fill the cache.
	reply := (&dns.Msg{}).SetRcode(request, dns.RcodeBadAlg)

	// We are testing that SERVFAIL responses aren't cached
	testCache.set(request, reply, upstreamWithAddr, testLogger)

	r, expired, _ := testCache.get(request)
	assert.Nil(t, r)
	assert.False(t, expired)
}

func TestCache_concurrent(t *testing.T) {
	testCache := newTestCache(t, nil)

	hosts := map[string]string{
		dns.Fqdn("yandex.com"):     "213.180.204.62",
		dns.Fqdn("google.com"):     "8.8.8.8",
		dns.Fqdn("www.google.com"): "8.8.4.4",
		dns.Fqdn("youtube.com"):    "173.194.221.198",
		dns.Fqdn("car.ru"):         "37.220.161.35",
		dns.Fqdn("cat.ru"):         "192.56.231.67",
	}

	g := &sync.WaitGroup{}
	g.Add(len(hosts))

	for k, v := range hosts {
		go setAndGetCache(t, testCache, g, k, v)
	}

	g.Wait()
}

const (
	// cacheTick is a cache check period.
	cacheTick = 100 * time.Millisecond

	// cacheTimeout is the timeout of cache check.
	cacheTimeout = 20 * cacheTick
)

func TestCacheExpiration(t *testing.T) {
	t.Parallel()

	dnsProxy := mustNew(t, &Config{
		Logger:         testLogger,
		UDPListenAddr:  []*net.UDPAddr{net.UDPAddrFromAddrPort(localhostAnyPort)},
		TCPListenAddr:  []*net.TCPAddr{net.TCPAddrFromAddrPort(localhostAnyPort)},
		UpstreamConfig: newTestUpstreamConfig(t, defaultTimeout, testDefaultUpstreamAddr),
		TrustedProxies: defaultTrustedProxies,
		CacheEnabled:   true,
	})

	servicetest.RequireRun(t, dnsProxy, testTimeout)

	// Create dns messages with TTL of 1 second.
	rrs := []dns.RR{
		newRR(t, "youtube.com.", dns.TypeA, 1, net.IP{173, 194, 221, 198}),
		newRR(t, "google.com.", dns.TypeA, 1, net.IP{8, 8, 8, 8}),
		newRR(t, "yandex.com.", dns.TypeA, 1, net.IP{213, 180, 204, 62}),
	}
	requests := make([]*dns.Msg, 0, len(rrs))
	responses := make([]*dns.Msg, 0, len(rrs))
	for _, rr := range rrs {
		req := (&dns.Msg{
			MsgHdr: dns.MsgHdr{
				Id: dns.Id(),
			},
		}).SetQuestion(rr.Header().Name, dns.TypeA)
		requests = append(requests, req)

		resp := (&dns.Msg{}).SetReply(req)
		resp.Answer = append(resp.Answer, rr)
		responses = append(responses, resp)

		dnsProxy.cache.set(req, resp, upstreamWithAddr, testLogger)
	}

	for i, r := range requests {
		ci, expired, key := dnsProxy.cache.get(r)
		require.NotNil(t, ci)

		assert.False(t, expired)
		assert.Equal(t, msgToKey(ci.m), key)

		requireEqualMsgs(t, ci.m, responses[i])
	}

	assert.Eventually(t, func() bool {
		for _, r := range requests {
			if ci, _, _ := dnsProxy.cache.get(r); ci != nil {
				return false
			}
		}

		return true
	}, cacheTimeout, cacheTick)
}

func TestCacheExpirationWithTTLOverride(t *testing.T) {
	var ans []dns.RR

	onExchange := newECSReplyHandler(&ans, nil, nil)
	u := newTestECSUpstream(onExchange)

	dnsProxy := mustNew(t, &Config{
		Logger:        testLogger,
		UDPListenAddr: []*net.UDPAddr{net.UDPAddrFromAddrPort(localhostAnyPort)},
		TCPListenAddr: []*net.TCPAddr{net.TCPAddrFromAddrPort(localhostAnyPort)},
		UpstreamConfig: &UpstreamConfig{
			Upstreams: []upstream.Upstream{u},
		},
		TrustedProxies: defaultTrustedProxies,
		CacheEnabled:   true,
		DNSSECEnabled:  true,
		CacheMinTTL:    20,
		CacheMaxTTL:    40,
	})

	servicetest.RequireRun(t, dnsProxy, testTimeout)

	d := &DNSContext{}

	t.Run("replace_min", func(t *testing.T) {
		d.Req = newHostTestMessage("host")
		d.Addr = netip.AddrPort{}

		ans = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{
				Rrtype: dns.TypeA,
				Name:   "host.",
				Ttl:    10,
			},
			A: net.IP{4, 3, 2, 1},
		}}

		err := dnsProxy.Resolve(testutil.ContextWithTimeout(t, defaultTimeout), d)
		require.NoError(t, err)

		ci, expired, key := dnsProxy.cache.get(d.Req)
		assert.False(t, expired)
		assert.Equal(t, msgToKey(d.Req), key)

		require.NotNil(t, ci)
		assert.Equal(t, dnsProxy.cacheMinTTL, ci.m.Answer[0].Header().Ttl)
	})

	t.Run("replace_max", func(t *testing.T) {
		d.Req = newHostTestMessage("host2")
		d.Addr = netip.AddrPort{}

		ans = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{
				Rrtype: dns.TypeA,
				Name:   "host2.",
				Ttl:    60,
			},
			A: net.IP{4, 3, 2, 1},
		}}

		err := dnsProxy.Resolve(testutil.ContextWithTimeout(t, defaultTimeout), d)
		require.NoError(t, err)

		ci, expired, key := dnsProxy.cache.get(d.Req)
		assert.False(t, expired)
		assert.Equal(t, msgToKey(d.Req), key)

		require.NotNil(t, ci)
		assert.Equal(t, dnsProxy.cacheMaxTTL, ci.m.Answer[0].Header().Ttl)
	})
}

type testEntry struct {
	q string
	a []dns.RR
	t uint16
}

type testCase struct {
	ok require.BoolAssertionFunc
	q  string
	a  []dns.RR
	t  uint16
}

type testCases struct {
	cache []testEntry
	cases []testCase
}

func TestCache(t *testing.T) {
	t.Run("simple", func(t *testing.T) {
		testCases{
			cache: []testEntry{{
				q: "google.com.",
				a: []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t: dns.TypeA,
			}},
			cases: []testCase{{
				ok: require.True,
				q:  "google.com.",
				a:  []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t:  dns.TypeA,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeMX,
			}},
		}.run(t)
	})

	t.Run("mixed_case", func(t *testing.T) {
		testCases{
			cache: []testEntry{{
				q: "gOOgle.com.",
				a: []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t: dns.TypeA,
			}},
			cases: []testCase{{
				ok: require.True,
				q:  "gOOgle.com.",
				a:  []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t:  dns.TypeA,
			}, {
				ok: require.True,
				q:  "google.com.",
				a:  []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t:  dns.TypeA,
			}, {
				ok: require.True,
				q:  "GOOGLE.COM.",
				a:  []dns.RR{newRR(t, "google.com.", dns.TypeA, 3600, net.IP{8, 8, 8, 8})},
				t:  dns.TypeA,
			}, {
				q:  "gOOgle.com.",
				t:  dns.TypeMX,
				ok: require.False,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeMX,
			}, {
				ok: require.False,
				q:  "GOOGLE.COM.",
				t:  dns.TypeMX,
			}},
		}.run(t)
	})

	t.Run("zero_ttl", func(t *testing.T) {
		testCases{
			cache: []testEntry{{
				q: "gOOgle.com.",
				a: []dns.RR{newRR(t, "google.com.", dns.TypeA, 0, net.IP{8, 8, 8, 8})},
				t: dns.TypeA,
			}},
			cases: []testCase{{
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeA,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeA,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeA,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeMX,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeMX,
			}, {
				ok: require.False,
				q:  "google.com.",
				t:  dns.TypeMX,
			}},
		}.run(t)
	})
}

func (tests testCases) run(t *testing.T) {
	testCache := newTestCache(t, nil)

	for _, res := range tests.cache {
		request := (&dns.Msg{}).SetQuestion(res.q, res.t)

		reply := (&dns.Msg{
			MsgHdr: dns.MsgHdr{
				Response: true,
			},
			Answer: res.a,
		}).SetReply(request)
		testCache.set(request, reply, upstreamWithAddr, testLogger)
	}

	for _, tc := range tests.cases {
		request := (&dns.Msg{}).SetQuestion(tc.q, tc.t)

		ci, expired, _ := testCache.get(request)
		assert.False(t, expired)
		tc.ok(t, ci != nil)

		if tc.a == nil {
			return
		} else if ci == nil {
			continue
		}

		reply := (&dns.Msg{
			MsgHdr: dns.MsgHdr{
				Response: true,
			},
			Answer: tc.a,
		}).SetQuestion(tc.q, tc.t)

		testCache.set(request, reply, upstreamWithAddr, testLogger)

		requireEqualMsgs(t, ci.m, reply)
	}
}

// requireEqualMsgs asserts the messages are equal except their ID, Rdlength, and
// the case of questions.
func requireEqualMsgs(tb testing.TB, expected, actual *dns.Msg) {
	tb.Helper()

	temp := *expected
	temp.Id = actual.Id

	require.Equal(tb, len(temp.Answer), len(actual.Answer))
	for i, ans := range actual.Answer {
		temp.Answer[i].Header().Rdlength = ans.Header().Rdlength
	}
	for _, rr := range actual.Answer {
		if a, ok := rr.(*dns.A); ok {
			if a4 := a.A.To4(); a4 != nil {
				a.A = a4
			}
		}
	}
	for i := range temp.Question {
		temp.Question[i].Name = strings.ToLower(temp.Question[i].Name)
	}
	for i := range actual.Question {
		actual.Question[i].Name = strings.ToLower(actual.Question[i].Name)
	}

	assert.Equal(tb, &temp, actual)
}

func setAndGetCache(t *testing.T, c *cache, g *sync.WaitGroup, host, ip string) {
	defer g.Done()

	ipAddr := net.ParseIP(ip)

	req := (&dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id: dns.Id(),
		},
	}).SetQuestion(host, dns.TypeA)

	dnsMsg := (&dns.Msg{
		MsgHdr: dns.MsgHdr{
			Response: true,
		},
		Answer: []dns.RR{newRR(t, host, dns.TypeA, 1, ipAddr)},
	}).SetQuestion(host, dns.TypeA)

	c.set(req, dnsMsg, upstreamWithAddr, testLogger)

	for range 2 {
		ci, expired, key := c.get(dnsMsg)
		require.NotNilf(t, ci, "no cache found for %s", host)

		assert.False(t, expired)
		assert.Equal(t, msgToKey(req), key)

		requireEqualMsgs(t, ci.m, dnsMsg)
	}

	assert.Eventuallyf(t, func() bool {
		ci, _, _ := c.get(req)

		return ci == nil
	}, cacheTimeout, cacheTick, "cache for %s should already be removed", host)
}

func TestCache_getWithSubnet(t *testing.T) {
	const testFQDN = "example.com."

	ip1234, ip2234, ip3234 := net.IP{1, 2, 3, 4}, net.IP{2, 2, 3, 4}, net.IP{3, 2, 3, 4}
	req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)
	mask16 := net.CIDRMask(16, netutil.IPv4BitLen)
	mask24 := net.CIDRMask(24, netutil.IPv4BitLen)

	c := newTestCache(t, &cacheConfig{withECS: true})

	t.Run("empty", func(t *testing.T) {
		ci, expired, _ := c.getWithSubnet(req, &net.IPNet{IP: ip1234, Mask: mask24})
		assert.Nil(t, ci)
		assert.False(t, expired)
	})

	// Add a response with subnet.
	resp := (&dns.Msg{
		Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 1, net.IP{1, 1, 1, 1})},
	}).SetReply(req)
	c.setWithSubnet(req, resp, upstreamWithAddr, &net.IPNet{IP: ip1234, Mask: mask16}, testLogger)

	t.Run("different_ip", func(t *testing.T) {
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{IP: ip2234, Mask: mask24})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, ip2234, 0), key)
		assert.Nil(t, ci)
	})

	// Add a response entry with subnet #2.
	resp = (&dns.Msg{
		Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 1, net.IP{2, 2, 2, 2})},
	}).SetReply(req)
	c.setWithSubnet(req, resp, upstreamWithAddr, &net.IPNet{IP: ip2234, Mask: mask16}, testLogger)

	// Add a response entry without subnet.
	resp = (&dns.Msg{
		Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 1, net.IP{3, 3, 3, 3})},
	}).SetReply(req)
	c.setWithSubnet(req, resp, upstreamWithAddr, &net.IPNet{IP: nil, Mask: nil}, testLogger)

	t.Run("with_subnet_1", func(t *testing.T) {
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{IP: ip1234, Mask: mask24})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, ip1234.Mask(mask16), 16), key)

		require.NotNil(t, ci)
		require.NotNil(t, ci.m)
		require.NotEmpty(t, ci.m.Answer)

		a := testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0])
		assert.True(t, a.A.Equal(net.IP{1, 1, 1, 1}))
	})

	t.Run("with_subnet_2", func(t *testing.T) {
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{IP: ip2234, Mask: mask24})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, ip2234.Mask(mask16), 16), key)

		require.NotNil(t, ci)
		require.NotNil(t, ci.m)
		require.NotEmpty(t, ci.m.Answer)

		a := testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0])
		assert.True(t, a.A.Equal(net.IP{2, 2, 2, 2}))
	})

	t.Run("with_subnet_3", func(t *testing.T) {
		// Note: In sharded cache, the IP used for retrieval must match the IP
		// used for insertion, because each IP hashes to a specific shard.
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{IP: ip3234, Mask: mask24})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, ip3234, 0), key)

		require.NotNil(t, ci)
		require.NotNil(t, ci.m)
		require.NotEmpty(t, ci.m.Answer)

		a := testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0])
		assert.True(t, a.A.Equal(net.IP{3, 3, 3, 3}))
	})
}

func TestCache_getWithSubnet_mask(t *testing.T) {
	const testFQDN = "example.com."

	testIP := net.IP{176, 112, 191, 0}
	noMatchIP := net.IP{177, 112, 191, 0}

	// cachedIP/cidrMask network contains the testIP.
	const cidrMaskOnes = 20
	cidrMask := net.CIDRMask(cidrMaskOnes, netutil.IPv4BitLen)
	cachedIP := net.IP{176, 112, 176, 0}

	ansIP := net.IP{4, 4, 4, 4}

	c := newTestCache(t, &cacheConfig{
		withECS:    true,
		optimistic: true,
	})

	req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)
	resp := (&dns.Msg{
		Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 300, ansIP)},
	}).SetReply(req)

	// Cache IP network that contains the testIP.
	c.setWithSubnet(
		req,
		resp,
		upstreamWithAddr,
		&net.IPNet{IP: cachedIP, Mask: cidrMask},
		testLogger,
	)

	t.Run("mask_matched", func(t *testing.T) {
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{
			IP:   testIP,
			Mask: net.CIDRMask(24, netutil.IPv4BitLen),
		})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, testIP.Mask(cidrMask), cidrMaskOnes), key)

		require.NotNil(t, ci)
		require.NotNil(t, ci.m)
		require.NotEmpty(t, ci.m.Answer)

		a := testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0])
		assert.True(t, a.A.Equal(ansIP))
	})

	t.Run("no_mask_matched", func(t *testing.T) {
		ci, expired, key := c.getWithSubnet(req, &net.IPNet{
			IP:   noMatchIP,
			Mask: net.CIDRMask(24, netutil.IPv4BitLen),
		})
		assert.False(t, expired)
		assert.Equal(t, msgToKeyWithSubnet(req, noMatchIP, 0), key)
		assert.Nil(t, ci)
	})
}

// TestCache_subnetIsolation asserts that the entries of the subnet cache are
// keyed by the client subnet, so that a response resolved for one subnet is
// never served to a different one.
//
// See https://github.com/asop-linu/dnsproxy/issues/.
func TestCache_subnetIsolation(t *testing.T) {
	const testFQDN = "example.com."

	netOf := func(ipStr string, ones int) (n *net.IPNet) {
		ip := net.ParseIP(ipStr)

		return &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, netutil.IPv4BitLen)}
	}

	respWith := func(ipStr string) (resp *dns.Msg) {
		req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)

		return (&dns.Msg{
			Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 300, net.ParseIP(ipStr))},
		}).SetReply(req)
	}

	answerIP := func(ci *cacheItem) (ip net.IP) {
		t.Helper()

		require.NotNil(t, ci)

		a := testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0])

		return a.A
	}

	// assertIP compares addresses with [net.IP.Equal], since [net.ParseIP]
	// returns a 16-byte representation, while an A record holds 4 bytes.
	assertIP := func(t *testing.T, want net.IP, ci *cacheItem) {
		t.Helper()

		if got := answerIP(ci); !want.Equal(got) {
			assert.Fail(t, "unexpected answer", "want %v, got %v", want, got)
		}
	}

	req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)
	c := newTestCache(t, &cacheConfig{withECS: true})

	netA := netOf("45.155.205.0", 24)
	netB := netOf("185.87.111.0", 24)
	ansA := net.ParseIP("45.142.246.128")
	ansB := net.ParseIP("135.181.102.167")

	t.Run("distinct_subnets_do_not_overwrite", func(t *testing.T) {
		c.setWithSubnet(req, respWith("45.142.246.128"), upstreamWithAddr, netA, testLogger)
		c.setWithSubnet(req, respWith("135.181.102.167"), upstreamWithAddr, netB, testLogger)

		ci, expired, _ := c.getWithSubnet(req, netA)
		assert.False(t, expired)
		assertIP(t, ansA, ci)

		ci, expired, _ = c.getWithSubnet(req, netB)
		assert.False(t, expired)
		assertIP(t, ansB, ci)
	})

	t.Run("host_bits_share_one_entry", func(t *testing.T) {
		// Two addresses from the same subnet must resolve to the same entry, no
		// matter which host bits the client puts into the request.
		ci, expired, _ := c.getWithSubnet(req, netOf("45.155.205.37", 24))
		assert.False(t, expired)
		assertIP(t, ansA, ci)

		ci, expired, _ = c.getWithSubnet(req, netOf("45.155.205.200", 24))
		assert.False(t, expired)
		assertIP(t, ansA, ci)
	})

	t.Run("unrelated_subnet_misses", func(t *testing.T) {
		// A subnet that was never cached must not fall back to any other
		// entry.
		ci, _, _ := c.getWithSubnet(req, netOf("91.199.242.0", 24))
		assert.Nil(t, ci)
	})

	t.Run("null_subnet_entry_not_written", func(t *testing.T) {
		// The old code stored the response of a non-echoing upstream under a
		// null subnet, which the longest-prefix-match lookup in getWithSubnet
		// then matched for every other subnet.  cacheResp no longer writes
		// such an entry, so the response must be reachable only through the
		// requested subnet.
		prx := mustNew(t, &Config{
			Logger:                 testLogger,
			UpstreamConfig:         &UpstreamConfig{Upstreams: []upstream.Upstream{upstreamWithAddr}},
			EnableEDNSClientSubnet: true,
			CacheEnabled:           true,
			CacheSizeBytes:         testCacheSize,
		})

		d := &DNSContext{
			Req:    (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA),
			Res:    respWith("45.142.246.128"),
			ReqECS: netA,
		}
		prx.cacheResp(d)

		ci, _, _ := prx.cache.getWithSubnet(d.Req, netA)
		require.NotNil(t, ci)
		assertIP(t, ansA, ci)

		// No null-subnet entry may exist, otherwise it would shadow every
		// other subnet through the longest-prefix-match lookup.
		ci, _, _ = prx.cache.getWithSubnet(d.Req, &net.IPNet{IP: nil, Mask: nil})
		assert.Nil(t, ci, "no null-subnet entry must exist")

		// And an unrelated subnet must not pick up this answer.
		ci, _, _ = prx.cache.getWithSubnet(d.Req, netOf("91.199.242.0", 24))
		assert.Nil(t, ci, "unrelated subnet must not match")
	})

	t.Run("zero_mask_no_panic", func(t *testing.T) {
		// A /0 subnet must not panic in the longest-prefix-match loop.
		assert.NotPanics(t, func() {
			c.getWithSubnet(req, &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, netutil.IPv4BitLen)})
		})
	})
}

// TestCache_getWithSubnet_lpm asserts the longest-prefix-match behavior between
// the subnets of different lengths.
func TestCache_getWithSubnet_lpm(t *testing.T) {
	const testFQDN = "example.com."

	req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)
	resp := (&dns.Msg{
		Answer: []dns.RR{newRR(t, testFQDN, dns.TypeA, 300, net.ParseIP("45.142.246.128"))},
	}).SetReply(req)

	c := newTestCache(t, &cacheConfig{withECS: true})
	c.setWithSubnet(req, resp, upstreamWithAddr, &net.IPNet{
		IP:   net.ParseIP("45.155.192.0"),
		Mask: net.CIDRMask(18, netutil.IPv4BitLen),
	}, testLogger)

	answerIP := func(ci *cacheItem) (ip net.IP) {
		t.Helper()

		require.NotNil(t, ci)

		return testutil.RequireTypeAssert[*dns.A](t, ci.m.Answer[0]).A
	}

	// assertIP compares addresses with [net.IP.Equal], since [net.ParseIP]
	// returns a 16-byte representation, while an A record holds 4 bytes.
	assertIP := func(t *testing.T, want net.IP, ci *cacheItem, msgAndArgs ...any) {
		t.Helper()

		got := answerIP(ci)
		if !want.Equal(got) {
			assert.Fail(t, "unexpected answer", "want %v, got %v", want, got)
		}
	}

	wantIP := net.ParseIP("45.142.246.128")

	t.Run("longer_mask_hits", func(t *testing.T) {
		for _, ones := range []int{19, 20, 24, 32} {
			ci, expired, _ := c.getWithSubnet(req, &net.IPNet{
				IP:   net.ParseIP("45.155.205.37"),
				Mask: net.CIDRMask(ones, netutil.IPv4BitLen),
			})
			assert.False(t, expired)
			assertIP(t, wantIP, ci, "mask /%d", ones)
		}
	})

	t.Run("outside_of_cached_subnet_misses", func(t *testing.T) {
		ci, _, _ := c.getWithSubnet(req, &net.IPNet{
			IP:   net.ParseIP("91.199.242.1"),
			Mask: net.CIDRMask(24, netutil.IPv4BitLen),
		})
		assert.Nil(t, ci)
	})
}

func TestCache_IsCacheable_negative(t *testing.T) {
	const someTTL = 3600

	msgHdr := func(rcode int) (hdr dns.MsgHdr) { return dns.MsgHdr{Id: dns.Id(), Rcode: rcode} }
	aQuestions := func(name string) []dns.Question {
		return []dns.Question{{
			Name:   name,
			Qtype:  dns.TypeA,
			Qclass: dns.ClassINET,
		}}
	}

	cnameAns := func(name, cname string) (rr dns.RR) {
		return &dns.CNAME{
			Hdr: dns.RR_Header{
				Name:   name,
				Rrtype: dns.TypeCNAME,
				Class:  dns.ClassINET,
				Ttl:    someTTL,
			},
			Target: cname,
		}
	}

	soaAns := func(name, ns, mbox string) (rr dns.RR) {
		return &dns.SOA{
			Hdr: dns.RR_Header{
				Name:   name,
				Rrtype: dns.TypeSOA,
				Class:  dns.ClassINET,
				Ttl:    someTTL,
			},
			Ns:   ns,
			Mbox: mbox,
		}
	}

	nsAns := func(name, ns string) (rr dns.RR) {
		return &dns.NS{
			Hdr: dns.RR_Header{
				Name:   name,
				Rrtype: dns.TypeNS,
				Class:  dns.ClassINET,
				Ttl:    someTTL,
			},
			Ns: ns,
		}
	}

	aAns := func(name string, a net.IP) (rr dns.RR) {
		return &dns.A{
			Hdr: dns.RR_Header{
				Name:   name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    someTTL,
			},
			A: a,
		}
	}

	const (
		hostname        = "AN.EXAMPLE."
		anotherHostname = "ANOTHER.EXAMPLE."
		cname           = "TRIPPLE.XX."
		mbox            = "HOSTMASTER.NS1.XX."
		ns1, ns2        = "NS1.XX.", "NS2.XX."
		xx              = "XX."
	)

	// See https://datatracker.ietf.org/doc/html/rfc2308.
	testCases := []struct {
		req     *dns.Msg
		name    string
		wantTTL uint32
	}{{
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeNameError),
			Question: aQuestions(hostname),
			Answer:   []dns.RR{cnameAns(hostname, cname)},
			Ns: []dns.RR{
				soaAns(xx, ns1, mbox),
				nsAns(xx, ns1),
				nsAns(xx, ns2),
			},
			Extra: []dns.RR{
				aAns(ns1, net.IP{127, 0, 0, 2}),
				aAns(ns2, net.IP{127, 0, 0, 3}),
			},
		},
		name:    "rfc2308_nxdomain_response_type_1",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeNameError),
			Question: aQuestions(hostname),
			Answer:   []dns.RR{cnameAns(hostname, cname)},
			Ns:       []dns.RR{soaAns("XX.", ns1, mbox)},
		},
		name:    "rfc2308_nxdomain_response_type_2",
		wantTTL: someTTL,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeNameError),
			Question: aQuestions(hostname),
			Answer:   []dns.RR{cnameAns(hostname, cname)},
		},
		name:    "rfc2308_nxdomain_response_type_3",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeNameError),
			Question: aQuestions(hostname),
			Answer:   []dns.RR{cnameAns(hostname, cname)},
			Ns: []dns.RR{
				nsAns(xx, ns1),
				nsAns(xx, ns2),
			},
			Extra: []dns.RR{
				aAns(ns1, net.IP{127, 0, 0, 2}),
				aAns(ns2, net.IP{127, 0, 0, 3}),
			},
		},
		name:    "rfc2308_nxdomain_response_type_4",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeSuccess),
			Question: aQuestions(hostname),
			Answer:   []dns.RR{cnameAns(hostname, cname)},
			Ns: []dns.RR{
				nsAns(xx, ns1),
				nsAns(xx, ns2),
			},
			Extra: []dns.RR{
				aAns(ns1, net.IP{127, 0, 0, 2}),
				aAns(ns2, net.IP{127, 0, 0, 3}),
			},
		},
		name:    "rfc2308_nxdomain_referral_response",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeSuccess),
			Question: aQuestions(anotherHostname),
			Ns: []dns.RR{
				soaAns(xx, ns1, mbox),
				nsAns(xx, ns1),
				nsAns(xx, ns2),
			},
			Extra: []dns.RR{
				aAns(ns1, net.IP{127, 0, 0, 2}),
				aAns(ns2, net.IP{127, 0, 0, 3}),
			},
		},
		name:    "rfc2308_nodata_response_type_1",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeSuccess),
			Question: aQuestions(anotherHostname),
			Ns:       []dns.RR{soaAns(xx, ns1, mbox)},
		},
		name:    "rfc2308_nodata_response_type_2",
		wantTTL: someTTL,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeSuccess),
			Question: aQuestions(anotherHostname),
		},
		name:    "rfc2308_nodata_response_type_3",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeSuccess),
			Question: aQuestions(anotherHostname),
			Ns: []dns.RR{
				nsAns(xx, ns1),
				nsAns(xx, ns2),
			},
			Extra: []dns.RR{
				aAns(ns1, net.IP{127, 0, 0, 2}),
				aAns(ns2, net.IP{127, 0, 0, 3}),
			},
		},
		name:    "rfc2308_nodata_referral_response",
		wantTTL: 0,
	}, {
		req: &dns.Msg{
			MsgHdr:   msgHdr(dns.RcodeServerFailure),
			Question: aQuestions(anotherHostname),
		},
		name:    "servfail_response",
		wantTTL: ServFailMaxCacheTTL,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantTTL, cacheTTL(tc.req, testLogger))
		})
	}
}
