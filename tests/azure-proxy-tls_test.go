package main

import (
	"crypto/tls"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The upstream proxy hardcodes sslmode=require. pgx's system root setting must
// still enforce both hostname and chain verification, with no plaintext fallback.
func TestProxySystemRootsRequireVerifiedTLS(t *testing.T) {
	bundle, err := filepath.Abs("../certifi/certifi/cacert.pem")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSSLROOTCERT", "system")
	t.Setenv("SSL_CERT_FILE", bundle)
	t.Setenv("SSL_CERT_DIR", t.TempDir())
	cfg, err := pgconn.ParseConfig("postgres://identity:token@server.postgres.database.azure.com:5432/juicefs?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	check := func(c *tls.Config) {
		t.Helper()
		if c == nil || c.InsecureSkipVerify || c.ServerName != "server.postgres.database.azure.com" || c.RootCAs == nil {
			t.Fatalf("upstream TLS does not verify server identity: %+v", c)
		}
	}
	check(cfg.TLSConfig)
	for _, fallback := range cfg.Fallbacks {
		check(fallback.TLSConfig)
	}
}
