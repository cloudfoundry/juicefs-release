package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/verify-backends/internal/backend"
)

type config struct {
	Postgres struct {
		Host, Port, Database, User, Password string
	} `json:"postgres"`
	Blobstore struct {
		Host, Port, User, Password, CA string
	} `json:"blobstore"`
}

func request(client backend.HTTPClient, method, url, user, password, body string, want ...int) error {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	if method == "PROPFIND" {
		req.Header.Set("Depth", "0")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	fmt.Printf("%s %s: %d\n", method, req.URL.Path, resp.StatusCode)
	for _, code := range want {
		if resp.StatusCode == code {
			return nil
		}
	}
	return fmt.Errorf("%s %s returned %d (expected %v)", method, req.URL.Path, resp.StatusCode, want)
}

func run(path string) error {
	return runWith(path, backend.Postgres{}, nil)
}

func runWith(path string, database backend.Database, httpClient backend.HTTPClient) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = database.Check(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", cfg.Postgres.User, cfg.Postgres.Password, cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database))
	if err != nil {
		return fmt.Errorf("postgres connection: %w", err)
	}
	fmt.Println("Postgres SELECT 1: OK")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.Blobstore.CA)) {
		return fmt.Errorf("invalid blobstore CA certificate")
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool,
		}}}
	}
	base := fmt.Sprintf("https://%s:%s/admin/", cfg.Blobstore.Host, cfg.Blobstore.Port)
	if err = request(client, "PROPFIND", base, cfg.Blobstore.User, cfg.Blobstore.Password, "", 207); err != nil {
		return err
	}
	// A unique path avoids touching other objects. DELETE runs even if GET fails.
	objectPath := fmt.Sprintf("%sjuicefs-verification-%d", base, time.Now().UnixNano())
	if err = request(client, "PUT", objectPath, cfg.Blobstore.User, cfg.Blobstore.Password, "juicefs-verification", 201, 204); err != nil {
		return err
	}
	defer request(client, "DELETE", objectPath, cfg.Blobstore.User, cfg.Blobstore.Password, "", 204)
	if err = request(client, "GET", objectPath, cfg.Blobstore.User, cfg.Blobstore.Password, "", 200); err != nil {
		return err
	}
	dir := objectPath + "-dir/"
	if err = request(client, "MKCOL", dir, cfg.Blobstore.User, cfg.Blobstore.Password, "", 201); err != nil {
		return fmt.Errorf("WebDAV directory creation: %w", err)
	}
	defer request(client, "DELETE", dir, cfg.Blobstore.User, cfg.Blobstore.Password, "", 204)
	fmt.Println("WebDAV backend: compatible with basic JuiceFS operations")
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verify-backends CONFIG.json")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
