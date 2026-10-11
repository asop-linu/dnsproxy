//go:build !integration

package proxy

// integrationEnabled tells the tests that require a working network whether
// they should run.
const integrationEnabled = false
