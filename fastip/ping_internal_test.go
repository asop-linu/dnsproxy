package fastip

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/logutil/slogutil"
	"github.com/AdguardTeam/golibs/netutil"
	"github.com/AdguardTeam/golibs/testutil"
	"github.com/asop-linu/dnsproxy/internal/dnsproxytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unit is the convenient alias for struct{}.
type unit = struct{}

func TestFastestAddr_PingAll_timeout(t *testing.T) {
	t.Run("isolated", func(t *testing.T) {
		f := New(&Config{Logger: slogutil.NewDiscardLogger()})

		// The hook must report failure, otherwise pingAll would find a
		// successful result instead of timing out.  Pre-fill the channel, as a
		// hook waiting for a send made after pingAll returns would only ever be
		// released by the timeout and would leak a goroutine.
		failCh := make(chan unit, len(f.pingPorts)*2)
		for range cap(failCh) {
			failCh <- unit{}
		}

		f.pinger.Control = func(_, _ string, _ syscall.RawConn) error {
			<-failCh

			return errors.Error("test: isolated")
		}

		ip := netutil.IPv4Localhost()
		res := f.pingAll("", []netip.Addr{ip, ip})
		require.Nil(t, res)
	})

	t.Run("cached", func(t *testing.T) {
		f := New(&Config{Logger: slogutil.NewDiscardLogger()})

		const lat uint = 42

		ip1 := netutil.IPv4Localhost()
		ip2 := netip.MustParseAddr("127.0.0.2")
		f.cacheAddSuccessful(ip1, lat)

		// Only ip2 is missing from the cache, so schedulePings must dial it
		// once per ping port.  Release every expected call up front: a hook
		// that waits for a send made after pingAll returns would instead
		// stall until firstSuccessRes times out and leak a goroutine.
		waitCh := make(chan unit, len(f.pingPorts))
		for range f.pingPorts {
			waitCh <- unit{}
		}

		var dialed atomic.Int64

		f.pinger.Control = func(_, address string, _ syscall.RawConn) error {
			dialed.Add(1)
			<-waitCh

			// Report a failure so that the only successful result is the one
			// taken from the cache.
			return errors.Error("test: unreachable")
		}

		res := f.pingAll("", []netip.Addr{ip1, ip2})
		require.NotNil(t, res)

		assert.True(t, res.success)
		assert.Equal(t, lat, res.latency)

		// ip1 must have been served from the cache rather than dialed again.
		assert.Equal(t, int64(len(f.pingPorts)), dialed.Load())
	})
}

// assertCaching checks the cache of f for containing a connection to ip with
// the specified status.
func assertCaching(t *testing.T, f *FastestAddr, ip netip.Addr, status int) {
	t.Helper()

	const tickDur = pingTCPTimeout / 16

	assert.Eventually(t, func() bool {
		ce := f.cacheFind(ip)

		return ce != nil && ce.status == status
	}, pingTCPTimeout, tickDur)
}

func TestFastestAddr_PingAll_cache(t *testing.T) {
	ip := netutil.IPv4Localhost()

	t.Run("cached_failed", func(t *testing.T) {
		f := New(&Config{Logger: slogutil.NewDiscardLogger()})
		f.cacheAddFailure(ip)

		res := f.pingAll("", []netip.Addr{ip, ip})
		require.Nil(t, res)
	})

	t.Run("cached_successful", func(t *testing.T) {
		const lat uint = 1

		f := New(&Config{Logger: slogutil.NewDiscardLogger()})
		f.cacheAddSuccessful(ip, lat)

		res := f.pingAll("", []netip.Addr{ip, ip})
		require.NotNil(t, res)
		assert.True(t, res.success)
		assert.Equal(t, lat, res.latency)
	})

	t.Run("not_cached", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		testutil.CleanupAndRequireSuccess(t, listener.Close)

		// Use distinct addresses.  schedulePings consults the cache per element
		// of ips, so repeating one address lets the first ping populate the
		// cache, after which the second element resolves from it and schedules
		// nothing.  The WaitGroup below would then wait for a dial that never
		// happens, which hung the whole package at roughly one run in twenty.
		ip1 := netutil.IPv4Localhost()
		ip2 := netip.MustParseAddr("127.0.0.2")

		f := New(&Config{Logger: slogutil.NewDiscardLogger()})

		f.pingPorts = []uint{uint(listener.Addr().(*net.TCPAddr).Port)}
		ips := []netip.Addr{ip1, ip2}
		hosts := []string{ip1.String(), ip2.String()}

		wg := &sync.WaitGroup{}
		wg.Add(len(ips) * len(f.pingPorts))

		f.pinger.Control = func(_, address string, _ syscall.RawConn) (err error) {
			hostport, err := netutil.ParseHostPort(address)
			require.NoError(t, err)

			assert.Contains(t, hosts, hostport.Host)
			assert.Contains(t, f.pingPorts, uint(hostport.Port))

			wg.Done()

			return nil
		}

		res := f.pingAll("", ips)
		require.NotNil(t, res)

		assert.True(t, res.success)

		// Wait for every dial before inspecting the cache, otherwise the entry
		// for the winning address may not be stored yet.
		wg.Wait()

		assertCaching(t, f, res.addrPort.Addr(), 0)
	})
}

// listen is a helper function that creates a new listener on ip for t.
func listen(t *testing.T, ip netip.Addr) (port uint) {
	t.Helper()

	l, err := net.Listen("tcp", netip.AddrPortFrom(ip, 0).String())
	require.NoError(t, err)
	testutil.CleanupAndRequireSuccess(t, l.Close)

	return uint(l.Addr().(*net.TCPAddr).Port)
}

func TestFastestAddr_PingAll(t *testing.T) {
	ip := netutil.IPv4Localhost()

	t.Run("single", func(t *testing.T) {
		f := New(&Config{Logger: slogutil.NewDiscardLogger()})
		res := f.pingAll("", []netip.Addr{ip})
		require.NotNil(t, res)

		assert.True(t, res.success)
		assert.Equal(t, ip, res.addrPort.Addr())
		// There was no ping so the port is zero.
		assert.Zero(t, res.addrPort.Port())

		// Nothing in the cache since there was no ping.
		ce := f.cacheFind(res.addrPort.Addr())
		require.Nil(t, ce)
	})

	t.Run("fastest", func(t *testing.T) {
		fastPort := listen(t, ip)
		slowPort := listen(t, ip)

		ctrlCh := make(chan unit, 1)

		f := New(&Config{Logger: slogutil.NewDiscardLogger()})
		f.pingPorts = []uint{
			fastPort,
			slowPort,
		}
		f.pinger.Control = func(_, address string, _ syscall.RawConn) error {
			addrPort := netip.MustParseAddrPort(address)
			require.Contains(t, []uint{fastPort, slowPort}, uint(addrPort.Port()))
			if addrPort.Port() == uint16(fastPort) {
				return nil
			}

			<-ctrlCh

			return nil
		}

		ips := []netip.Addr{ip, ip}
		res := f.pingAll("", ips)
		ctrlCh <- unit{}

		require.NotNil(t, res)

		assert.True(t, res.success)
		assert.Equal(t, ip, res.addrPort.Addr())
		assert.EqualValues(t, fastPort, res.addrPort.Port())

		assertCaching(t, f, ip, 0)
	})

	t.Run("zero", func(t *testing.T) {
		res := New(&Config{Logger: slogutil.NewDiscardLogger()}).pingAll("", nil)
		require.Nil(t, res)
	})

	t.Run("fail", func(t *testing.T) {
		port := dnsproxytest.NewFreePort(t)

		f := New(&Config{Logger: slogutil.NewDiscardLogger()})
		f.pingPorts = []uint{port}

		res := f.pingAll("test", []netip.Addr{ip, ip})
		require.Nil(t, res)

		assertCaching(t, f, ip, 1)
	})
}
