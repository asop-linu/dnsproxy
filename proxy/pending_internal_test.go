package proxy

import (
	"net"
	"testing"

	"github.com/AdguardTeam/golibs/netutil"
	"github.com/AdguardTeam/golibs/testutil"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingKeyFor returns the key that [defaultPendingRequests] uses for dctx.  It
// mirrors the construction in queue and done.
func pendingKeyFor(tb testing.TB, req *dns.Msg, ecs *net.IPNet) (key []byte) {
	tb.Helper()

	if ecs == nil {
		return msgToKey(req)
	}

	ones, _ := ecs.Mask.Size()

	return msgToKeyWithSubnet(req, ecs.IP.Mask(ecs.Mask), ones)
}

// TestDefaultPendingRequests_ecsKey asserts that the pending requests key is
// derived from the client subnet with the host bits masked off, so that two
// clients from the same subnet share a key, and it matches the subnet cache
// key.
func TestDefaultPendingRequests_ecsKey(t *testing.T) {
	const testFQDN = "example.com."

	netOf := func(ipStr string, ones int) (n *net.IPNet) {
		return &net.IPNet{
			IP:   net.ParseIP(ipStr),
			Mask: net.CIDRMask(ones, netutil.IPv4BitLen),
		}
	}

	req := (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA)

	t.Run("host_bits_do_not_affect_key", func(t *testing.T) {
		want := pendingKeyFor(t, req, netOf("45.155.205.0", 24))

		// Two clients from the same subnet, differing only in the host bits,
		// must produce the same key.
		for _, ipStr := range []string{"45.155.205.0", "45.155.205.37", "45.155.205.255"} {
			got := pendingKeyFor(t, req, netOf(ipStr, 24))
			assert.Equal(t, want, got, "ip %s", ipStr)
		}
	})

	t.Run("different_subnets_differ", func(t *testing.T) {
		a := pendingKeyFor(t, req, netOf("45.155.205.0", 24))
		b := pendingKeyFor(t, req, netOf("185.87.111.0", 24))

		assert.NotEqual(t, a, b)
	})

	t.Run("matches_cache_key", func(t *testing.T) {
		// The pending key must be identical to the subnet cache key, otherwise
		// the two subsystems disagree about what identifies a request.
		ecs := netOf("45.155.205.37", 24)
		ones, _ := ecs.Mask.Size()

		assert.Equal(
			t,
			msgToKeyWithSubnet(req, ecs.IP.Mask(ecs.Mask), ones),
			pendingKeyFor(t, req, ecs),
		)
	})

	t.Run("no_ecs_uses_plain_key", func(t *testing.T) {
		assert.Equal(t, msgToKey(req), pendingKeyFor(t, req, nil))
	})
}

// TestDefaultPendingRequests_ecsDedup asserts that two requests from the same
// client subnet are stored under the same pending key, even when their subnet
// addresses carry different host bits, so that the second one is recognized as
// a duplicate of the in-flight first.
func TestDefaultPendingRequests_ecsDedup(t *testing.T) {
	const testFQDN = "example.com."

	pr := newDefaultPendingRequests()
	ctx := testutil.ContextWithTimeout(t, defaultTimeout)

	newCtx := func(ipStr string) (d *DNSContext) {
		return &DNSContext{
			Req: (&dns.Msg{}).SetQuestion(testFQDN, dns.TypeA),
			ReqECS: &net.IPNet{
				IP:   net.ParseIP(ipStr),
				Mask: net.CIDRMask(24, netutil.IPv4BitLen),
			},
		}
	}

	// Queue the first request and inspect the key it was stored under.  This
	// avoids racing with a second goroutine, since the pending entry is keyed
	// by the subnet with the host bits masked off.
	first := newCtx("45.155.205.1")
	loaded, err := pr.queue(ctx, first)
	require.NoError(t, err)
	require.False(t, loaded, "the first request must not be a duplicate")

	storedKey := string(pendingKeyFor(t, first.Req, first.ReqECS))
	_, ok := pr.storage.Load(storedKey)
	require.True(t, ok, "the first request must be stored under its masked key")

	// A different host address from the same subnet must map onto that very
	// key, which is what makes the requests deduplicate.
	second := newCtx("45.155.205.2")
	secondKey := string(pendingKeyFor(t, second.Req, second.ReqECS))
	assert.Equal(t, storedKey, secondKey, "the same subnet must produce the same key")

	_, loaded = pr.storage.LoadOrStore(secondKey, &pendingRequest{
		finish: make(chan struct{}),
	})
	assert.True(t, loaded, "the same subnet must be seen as a duplicate")

	// A different subnet must not collide with it.
	other := newCtx("185.87.111.2")
	_, loaded = pr.storage.LoadOrStore(string(pendingKeyFor(t, other.Req, other.ReqECS)), &pendingRequest{
		finish: make(chan struct{}),
	})
	assert.False(t, loaded, "a different subnet must use a different key")

	first.Res = (&dns.Msg{}).SetReply(first.Req)
	pr.done(ctx, first, nil)

	// After done, the key must be released.
	_, ok = pr.storage.Load(storedKey)
	assert.False(t, ok, "the pending entry must be removed once done")
}
