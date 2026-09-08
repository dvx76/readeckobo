package main

// TLS trust for the on-device agent.
//
// The Kobo firmware (4.38) has no system CA store (/etc/ssl/certs does not
// exist), so a stock Go binary cannot verify any HTTPS server:
// "x509: certificate signed by unknown authority". To fix that we embed the
// Mozilla CA bundle and register it as Go's *fallback* root set:
//
//	x509.SetFallbackRoots(pool)
//
// Fallback roots are used only when no system roots are available (exactly
// the Kobo case) and are ignored when a system pool is present — so the
// embedded bundle never overrides a real trust store.
//
// The bundle (cabundle.pem) is the Mozilla CA list as packaged by curl.se:
//
//	source:  https://curl.se/ca/cacert.pem
//	fetched: 2026-09-08 (121 certificates, incl. ISRG Root X1)
//
// It is CRITICAL, not optional: without it every HTTPS request fails on the
// device. If the embedded file ever fails to parse, refuse to start rather
// than silently running with no trust.
//
// To refresh: `make refresh-cabundle` (re-downloads from curl.se), then
// commit the updated cmd/readeckobo-agent/cabundle.pem.

import (
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"os"
)

//go:embed cabundle.pem
var cabundlePEM []byte

// embeddedRoots is the parsed Mozilla bundle, registered as Go's fallback
// root set in init(). Exported to the package so tests can assert on it.
var embeddedRoots *x509.CertPool

func init() {
	pool, err := parseCertPool(cabundlePEM)
	if err != nil {
		// Never silently skip: without fallback roots the agent cannot
		// verify a single HTTPS server on the Kobo. Fail loudly at startup.
		fmt.Fprintf(os.Stderr, "readeckobo-agent: FATAL: embedded CA bundle (cabundle.pem) failed to parse: %v; refusing to run without TLS trust\n", err)
		os.Exit(1)
	}
	embeddedRoots = pool
	x509.SetFallbackRoots(embeddedRoots)
}

// parseCertPool builds a CertPool from PEM-encoded certificates. It fails
// (rather than returning an empty pool) when nothing parses, so a bad bundle
// is always a loud error at the call site.
func parseCertPool(pemData []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, errors.New("no valid PEM certificates found")
	}
	return pool, nil
}

// caPoolFromFile loads a PEM CA bundle from disk (the CA_FILE config key,
// for servers behind a private CA). Missing, unreadable or unparsable files
// are errors — never silently ignored.
func caPoolFromFile(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read CA bundle: %w", err)
	}
	pool, err := parseCertPool(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse CA bundle: %w", err)
	}
	return pool, nil
}
