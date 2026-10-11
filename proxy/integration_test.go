package proxy

import "testing"

// requireIntegration skips tb unless the integration build tag was set.
//
// Tests guarded by it talk to public DNS servers, so they fail whenever the
// network is unavailable or slow, or when a remote server rate-limits the
// repeated queries a test sends.  Such failures are indistinguishable from
// races in the code under test, while they are really a network dependency.
//
// Run the guarded tests with:
//
//	go test -tags=integration ./...
func requireIntegration(tb testing.TB) {
	tb.Helper()

	if !integrationEnabled {
		tb.Skip("requires network; run with -tags=integration")
	}
}
