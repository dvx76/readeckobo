package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Embedded Mozilla CA bundle

func TestEmbeddedCABundle(t *testing.T) {
	// SetFallbackRoots was called in init() with this pool — it cannot be
	// queried directly, but the parsed pool must be registered and non-nil,
	// and parsing errors are impossible here (init() would have os.Exit'ed).
	if embeddedRoots == nil {
		t.Fatal("embeddedRoots is nil: fallback roots were not registered")
	}

	subjects := embeddedRoots.Subjects()
	if len(subjects) <= 100 {
		t.Fatalf("embedded bundle has %d certificates, want > 100", len(subjects))
	}
	t.Logf("embedded CA bundle: %d certificates", len(subjects))

	// The bundle must contain at least one well-known public root that the
	// readeckobo server certs chain up to (ISRG Root X1, Let's Encrypt).
	found := false
	for _, der := range subjects {
		var rdn pkix.RDNSequence
		if _, err := asn1.Unmarshal(der, &rdn); err != nil {
			continue
		}
		var name pkix.Name
		name.FillFromRDNSequence(&rdn)
		if name.CommonName == "ISRG Root X1" || strings.Contains(strings.Join(name.Organization, ","), "Internet Security Research Group") {
			found = true
			break
		}
	}
	if !found {
		t.Error("embedded bundle does not contain ISRG Root X1 (or another Internet Security Research Group root)")
	}

	// Sanity: the bundle file parses to the same certificate count as the
	// pool (guards against a pool built from garbage-but-partially-valid PEM).
	blockCount := 0
	rest := cabundlePEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			blockCount++
		}
	}
	if blockCount != len(subjects) {
		t.Errorf("bundle PEM has %d CERTIFICATE blocks but the pool has %d subjects", blockCount, len(subjects))
	}
}

// ---------------------------------------------------------------------------
// CA_FILE behavior: private-CA HTTPS server

type testCA struct {
	rootPEM    []byte
	leafPEM    []byte
	leafKeyPEM []byte
}

// newTestCA generates a self-signed ECDSA root CA and a server certificate
// (signed by it, valid for 127.0.0.1/localhost) entirely in-test.
func newTestCA(t *testing.T, rootCN string) testCA {
	t.Helper()
	now := time.Now()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: rootCN, Organization: []string{"Test Org"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-server"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootTmpl, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		rootPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}),
		leafPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		leafKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER}),
	}
}

// newTLSServer serves a minimal /api/agent/state stand-in over TLS with the
// test CA's leaf certificate.
func newTLSServer(t *testing.T, ca testCA) *httptest.Server {
	t.Helper()
	cert, err := tls.X509KeyPair(ca.leafPEM, ca.leafKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, stateResponse{Articles: []Article{}})
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // client-side cert rejection logs "TLS handshake error" otherwise
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPoolOrDie(t, ca.rootPEM), // client-cert verification side, per test spec
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func caPoolOrDie(t *testing.T, pemData []byte) *x509.CertPool {
	t.Helper()
	pool, err := parseCertPool(pemData)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// writeConfigFile writes a minimal KEY=VALUE config and returns its path.
func writeConfigFile(t *testing.T, dir, serverURL, caFile string) string {
	t.Helper()
	lines := []string{"SERVER_URL=" + serverURL, "TOKEN=t", "DEVICE_SERIAL=s"}
	if caFile != "" {
		lines = append(lines, "CA_FILE="+caFile)
	}
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// agentFromConfig loads the config from disk and builds an agent with the
// *default* http.Client construction (no injected httpc), so CA_FILE wiring
// through newAgent → apiClient is exercised end to end.
func agentFromConfig(t *testing.T, configPath string) *Agent {
	t.Helper()
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	logger, err := newLogger("", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	ag, err := newAgent(agentOptions{
		cfg: cfg, logger: logger, dataDir: filepath.Dir(configPath), noRescan: true,
	})
	if err != nil {
		t.Fatalf("newAgent: %v", err)
	}
	return ag
}

// TestCAFileTrustsConfiguredPrivateCA is the happy path: with CA_FILE
// pointing at the private CA, the agent's client verifies the TLS server.
func TestCAFileTrustsConfiguredPrivateCA(t *testing.T) {
	ca := newTestCA(t, "Test Root CA")
	srv := newTLSServer(t, ca)

	dir := t.TempDir()
	caFilePath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFilePath, ca.rootPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ag := agentFromConfig(t, writeConfigFile(t, dir, srv.URL, caFilePath))

	articles, err := ag.client.fetchState(context.Background(), "s")
	if err != nil {
		t.Fatalf("fetchState over TLS with correct CA_FILE failed: %v", err)
	}
	if len(articles) != 0 {
		t.Errorf("articles = %+v, want empty", articles)
	}
}

// TestCAFileRejectsOtherCA: a CA_FILE that is a *different* (valid) CA must
// fail with a certificate verification error — the private CA replaces the
// trust set, it does not silently pass.
func TestCAFileRejectsOtherCA(t *testing.T) {
	ca := newTestCA(t, "Server Root CA")
	srv := newTLSServer(t, ca)
	other := newTestCA(t, "Unrelated Root CA")

	dir := t.TempDir()
	otherPath := filepath.Join(dir, "other-ca.pem")
	if err := os.WriteFile(otherPath, other.rootPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ag := agentFromConfig(t, writeConfigFile(t, dir, srv.URL, otherPath))

	_, err := ag.client.fetchState(context.Background(), "s")
	if err == nil {
		t.Fatal("fetchState should fail when the client trusts a different CA")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate verification error", err)
	}
}

// TestCAFileBadPathFailsConfigValidation: missing, unreadable and
// unparsable CA_FILE paths are loud config validation errors.
func TestCAFileBadPathFailsConfigValidation(t *testing.T) {
	cfgPath := func(dir, caFile string) string {
		return writeConfigFile(t, dir, "https://example.com", caFile)
	}

	// Missing file.
	dir := t.TempDir()
	if _, err := loadConfig(cfgPath(dir, filepath.Join(dir, "nope.pem"))); err == nil || !strings.Contains(err.Error(), "CA_FILE") {
		t.Errorf("missing CA_FILE err = %v, want CA_FILE error", err)
	}

	// Unparsable file (garbage, no PEM blocks).
	dir = t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	os.WriteFile(garbage, []byte("this is not a certificate\n"), 0o644)
	if _, err := loadConfig(cfgPath(dir, garbage)); err == nil || !strings.Contains(err.Error(), "CA_FILE") {
		t.Errorf("garbage CA_FILE err = %v, want CA_FILE error", err)
	}

	// Empty file parses as "no valid PEM certificates".
	dir = t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	os.WriteFile(empty, nil, 0o644)
	if _, err := loadConfig(cfgPath(dir, empty)); err == nil || !strings.Contains(err.Error(), "CA_FILE") {
		t.Errorf("empty CA_FILE err = %v, want CA_FILE error", err)
	}
}
