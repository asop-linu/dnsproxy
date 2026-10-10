package upstream

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/testutil"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDNSServerBindAttempts is the maximum number of attempts to find a port
// that is free for both UDP and TCP.
const testDNSServerBindAttempts = 100

func TestUpstream_plainDNS(t *testing.T) {
	srv := startDNSServer(t, func(w dns.ResponseWriter, req *dns.Msg) {
		resp := respondToTestMessage(req)

		err := w.WriteMsg(resp)

		pt := testutil.PanicT{}
		require.NoError(pt, err)
	})
	testutil.CleanupAndRequireSuccess(t, srv.Close)

	addr := fmt.Sprintf("127.0.0.1:%d", srv.port)
	u, err := AddressToUpstream(addr, &Options{
		Logger: testLogger,
	})
	require.NoError(t, err)
	testutil.CleanupAndRequireSuccess(t, u.Close)

	for range 10 {
		checkUpstream(t, u, addr)
	}
}

func TestUpstream_plainDNS_badID(t *testing.T) {
	req := createTestMessage()
	badIDResp := respondToTestMessage(req)
	badIDResp.Id++

	srv := startDNSServer(t, func(w dns.ResponseWriter, _ *dns.Msg) {
		require.NoError(testutil.PanicT{}, w.WriteMsg(badIDResp))
	})
	testutil.CleanupAndRequireSuccess(t, srv.Close)

	addr := fmt.Sprintf("127.0.0.1:%d", srv.port)
	u, err := AddressToUpstream(addr, &Options{
		Logger: testLogger,
		// Use a shorter timeout to speed up the test.
		Timeout: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	testutil.CleanupAndRequireSuccess(t, u.Close)

	resp, err := u.Exchange(req)

	var netErr net.Error
	require.ErrorAs(t, err, &netErr)

	assert.True(t, netErr.Timeout())
	assert.Nil(t, resp)
}

func TestUpstream_plainDNS_fallbackToTCP(t *testing.T) {
	req := createTestMessage()
	goodResp := respondToTestMessage(req)

	truncResp := goodResp.Copy()
	truncResp.Truncated = true

	badQNameResp := goodResp.Copy()
	badQNameResp.Question[0].Name = "bad." + req.Question[0].Name

	badQTypeResp := goodResp.Copy()
	badQTypeResp.Question[0].Qtype = dns.TypeCNAME

	testCases := []struct {
		udpResp *dns.Msg
		name    string
		wantUDP int
		wantTCP int
	}{{
		udpResp: goodResp,
		name:    "all_right",
		wantUDP: 1,
		wantTCP: 0,
	}, {
		udpResp: truncResp,
		name:    "truncated_response",
		wantUDP: 1,
		wantTCP: 1,
	}, {
		udpResp: badQNameResp,
		name:    "bad_qname",
		wantUDP: 1,
		wantTCP: 1,
	}, {
		udpResp: badQTypeResp,
		name:    "bad_qtype",
		wantUDP: 1,
		wantTCP: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var udpReqNum, tcpReqNum atomic.Uint32
			srv := startDNSServer(t, func(w dns.ResponseWriter, _ *dns.Msg) {
				var resp *dns.Msg
				if w.RemoteAddr().Network() == networkUDP {
					udpReqNum.Add(1)
					resp = tc.udpResp
				} else {
					tcpReqNum.Add(1)
					resp = goodResp
				}

				require.NoError(testutil.PanicT{}, w.WriteMsg(resp))
			})
			testutil.CleanupAndRequireSuccess(t, srv.Close)

			addr := fmt.Sprintf("127.0.0.1:%d", srv.port)
			u, err := AddressToUpstream(addr, &Options{
				Logger: testLogger,
				// Use a shorter timeout to speed up the test.
				Timeout: 100 * time.Millisecond,
			})
			require.NoError(t, err)
			testutil.CleanupAndRequireSuccess(t, u.Close)

			resp, err := u.Exchange(req)
			require.NoError(t, err)
			requireResponse(t, req, resp)

			assert.Equal(t, tc.wantUDP, int(udpReqNum.Load()))
			assert.Equal(t, tc.wantTCP, int(tcpReqNum.Load()))
		})
	}
}

// testDNSServer is a simple DNS server that can be used in unit-tests.
type testDNSServer struct {
	udpListener net.PacketConn
	tcpListener net.Listener
	udpSrv      *dns.Server
	tcpSrv      *dns.Server
	port        int
}

// type check
var _ io.Closer = (*testDNSServer)(nil)

// startDNSServer a test DNS server.
func startDNSServer(tb testing.TB, handler dns.HandlerFunc) (s *testDNSServer) {
	tb.Helper()

	s = &testDNSServer{}

	// The server must listen on the same port for both UDP and TCP, since the
	// tests use it to exercise the fallback between them.  Asking the kernel
	// for a free UDP port does not guarantee that the same number is free for
	// TCP, so retry until both transports are available on one port.  Without
	// this, the test fails with "bind: address already in use" whenever the
	// ephemeral port picked for UDP is already taken for TCP, which happens
	// intermittently when tests are run repeatedly or in parallel.
	for range testDNSServerBindAttempts {
		udpListener, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(tb, err)

		port := testutil.RequireTypeAssert[*net.UDPAddr](tb, udpListener.LocalAddr()).Port

		tcpListener, tcpErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if tcpErr != nil {
			require.NoError(tb, udpListener.Close())

			continue
		}

		s.port = port
		s.udpListener = udpListener
		s.tcpListener = tcpListener

		break
	}

	require.NotNilf(tb, s.udpListener, "failed to bind a port free for both UDP and TCP")

	s.udpSrv = &dns.Server{
		PacketConn: s.udpListener,
		Handler:    handler,
	}

	s.tcpSrv = &dns.Server{
		Listener: s.tcpListener,
		Handler:  handler,
	}

	go func() {
		pt := testutil.PanicT{}
		require.NoError(pt, s.udpSrv.ActivateAndServe())
	}()

	go func() {
		pt := testutil.PanicT{}
		require.NoError(pt, s.tcpSrv.ActivateAndServe())
	}()

	return s
}

// Close implements the io.Closer interface for *testDNSServer.
func (s *testDNSServer) Close() (err error) {
	udpErr := s.udpSrv.Shutdown()
	tcpErr := s.tcpSrv.Shutdown()

	return errors.Join(udpErr, tcpErr)
}
