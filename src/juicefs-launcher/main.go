// juicefs-launcher runs the JuiceFS CLI with the backend configuration,
// credentials, and CA trust rendered by BOSH.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cloudfoundry/juicefs-release/src/juicefs-launcher/internal/command"
)

type config struct {
	Metadata struct {
		Type, Host, Database, User, Password, Options string
		Port                                          int
	} `json:"metadata"`
	Storage struct {
		Type, Bucket            string
		AccessKey               string `json:"access_key"`
		SecretKey               string `json:"secret_key"`
		SessionToken            string `json:"session_token"`
		CACert                  string `json:"ca_cert"`
		CAFile                  string
		ManagedIdentity         bool   `json:"managed_identity"`
		ManagedIdentityClientID string `json:"managed_identity_client_id"`
	} `json:"storage"`
	Postgres struct {
		Host, Port, Database, User, Password string
	} `json:"postgres"`
	Blobstore struct {
		Hostname, Port, User, Password, CAFile string
	} `json:"blobstore"`
	Volume struct {
		Name, MountPoint, CacheDir, Subdir string
	} `json:"volume"`
}

func metadataURL(cfg config) (string, error) {
	m := cfg.Metadata
	if m.Type == "" {
		m.Type, m.Host, m.Database, m.User, m.Password, m.Options = "postgres", cfg.Postgres.Host, cfg.Postgres.Database, cfg.Postgres.User, cfg.Postgres.Password, "sslmode=disable"
		_, _ = fmt.Sscan(cfg.Postgres.Port, &m.Port)
	}
	if m.Host == "" || m.User == "" || m.Database == "" {
		return "", fmt.Errorf("metadata host, user and database are required")
	}
	if m.Port == 0 {
		if m.Type == "mysql" {
			m.Port = 3306
		} else {
			m.Port = 5432
		}
	}
	host := net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
	var result string
	switch m.Type {
	case "postgres":
		result = (&url.URL{Scheme: "postgres", User: url.UserPassword(m.User, ""), Host: host, Path: "/" + m.Database}).String()
	case "mysql":
		if strings.ContainsAny(m.User, ":@/()") || strings.ContainsAny(m.Database, "?/#") {
			return "", fmt.Errorf("invalid MySQL user or database")
		}
		result = fmt.Sprintf("mysql://%s:@(%s)/%s", m.User, host, m.Database)
	default:
		return "", fmt.Errorf("unsupported metadata type %q", m.Type)
	}
	if m.Options != "" {
		result += "?" + m.Options
	}
	return result, nil
}

func validateStorage(kind, bucket string) error {
	if kind != "webdav" && kind != "s3" && kind != "wasb" {
		return fmt.Errorf("unsupported storage type %q", kind)
	}
	endpoint, err := url.Parse(bucket)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return fmt.Errorf("storage bucket must be an HTTP(S) URL")
	}
	return nil
}

// storageEnvironment makes managed identity explicit and excludes credential
// sources that would otherwise take precedence in the upstream Azure client.
func storageEnvironment(cfg config, inherited []string) ([]string, error) {
	if !cfg.Storage.ManagedIdentity {
		if cfg.Storage.ManagedIdentityClientID != "" {
			return nil, fmt.Errorf("managed_identity_client_id requires managed_identity")
		}
		return inherited, nil
	}
	if cfg.Storage.Type != "wasb" || !strings.HasPrefix(cfg.Storage.Bucket, "https://") {
		return nil, fmt.Errorf("managed identity requires wasb storage with an HTTPS bucket")
	}
	if cfg.Storage.AccessKey == "" || cfg.Storage.SecretKey != "" || cfg.Storage.SessionToken != "" {
		return nil, fmt.Errorf("managed identity requires the Azure account name and no account key or SAS token")
	}
	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		key, value, _ := strings.Cut(entry, "=")
		if key == "AZURE_STORAGE_CONNECTION_STRING" && value != "" {
			return nil, fmt.Errorf("AZURE_STORAGE_CONNECTION_STRING conflicts with managed identity")
		}
		if key != "AZURE_CLIENT_ID" && key != "AZURE_TOKEN_CREDENTIALS" {
			env = append(env, entry)
		}
	}
	env = append(env, "AZURE_TOKEN_CREDENTIALS=ManagedIdentityCredential")
	if cfg.Storage.ManagedIdentityClientID != "" {
		env = append(env, "AZURE_CLIENT_ID="+cfg.Storage.ManagedIdentityClientID)
	}
	return env, nil
}

func run(mode, configPath string, extra ...string) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	return runWith(mode, configPath, command.ExecRunner{}, signals, checkFuse, extra...)
}

func checkFuse() error {
	fuse, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/fuse: %w", err)
	}
	return fuse.Close()
}

func runWith(mode, configPath string, runner command.Runner, signals <-chan os.Signal, deviceCheck func() error, extra ...string) error {
	contents, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(contents, &cfg); err != nil {
		return err
	}
	if mode == "mount" && cfg.Volume.Subdir != "" {
		if !strings.HasPrefix(cfg.Volume.Subdir, "/cf-volumes/") || strings.Contains(cfg.Volume.Subdir, "..") {
			return fmt.Errorf("invalid JuiceFS subdirectory %q", cfg.Volume.Subdir)
		}
	}

	metaURL, err := metadataURL(cfg)
	if err != nil {
		return err
	}
	if cfg.Metadata.Type == "" {
		cfg.Metadata.Password = cfg.Postgres.Password
	}
	if cfg.Storage.Type == "" {
		cfg.Storage.Type, cfg.Storage.Bucket = "webdav", fmt.Sprintf("https://%s:%s/admin/", cfg.Blobstore.Hostname, cfg.Blobstore.Port)
		cfg.Storage.AccessKey, cfg.Storage.SecretKey, cfg.Storage.CAFile = cfg.Blobstore.User, cfg.Blobstore.Password, cfg.Blobstore.CAFile
	}
	if err := validateStorage(cfg.Storage.Type, cfg.Storage.Bucket); err != nil {
		return err
	}
	var args []string
	switch mode {
	case "format":
		args = []string{"format", "--no-update", "--storage", cfg.Storage.Type, "--bucket", cfg.Storage.Bucket, metaURL, cfg.Volume.Name}
	case "mount":
		args = []string{"mount", "--foreground", "--subdir", cfg.Volume.Subdir, "--cache-dir", cfg.Volume.CacheDir, "--cache-size", "1024", "--backup-meta", "0", "--no-usage-report", metaURL, cfg.Volume.MountPoint}
	case "quota":
		if len(extra) != 3 || !strings.HasPrefix(extra[0], "/cf-volumes/") || strings.Contains(extra[0], "..") {
			return fmt.Errorf("quota requires instance path, capacity and inodes")
		}
		args = []string{"quota", "set", "--path", extra[0], "--capacity", extra[1], "--inodes", extra[2], metaURL}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	env, err := storageEnvironment(cfg, os.Environ())
	if err != nil {
		return err
	}
	env = append(env, "META_PASSWORD="+cfg.Metadata.Password, "ACCESS_KEY="+cfg.Storage.AccessKey, "SECRET_KEY="+cfg.Storage.SecretKey, "SESSION_TOKEN="+cfg.Storage.SessionToken)
	// The release owns its default trust roots rather than relying on stemcell files.
	env = append(env, "SSL_CERT_FILE=/var/vcap/packages/ca-certificates/certs/ca-certificates.crt", "SSL_CERT_DIR=/var/vcap/packages/ca-certificates/no-system-certs")
	if cfg.Storage.CACert != "" && cfg.Storage.CAFile != "" {
		env = append(env, "SSL_CERT_FILE="+cfg.Storage.CAFile)
	} else if cfg.Blobstore.CAFile != "" {
		env = append(env, "SSL_CERT_FILE="+cfg.Blobstore.CAFile)
	}
	if mode == "mount" {
		if err := deviceCheck(); err != nil {
			return err
		}
	}
	return runner.Run("/var/vcap/packages/juicefs/bin/juicefs", args, env, signals)
}

func main() {
	if len(os.Args) < 3 || (os.Args[1] != "format" && os.Args[1] != "mount" && os.Args[1] != "quota") {
		fmt.Fprintln(os.Stderr, "usage: juicefs-launcher format|mount|quota CONFIG.json [PATH CAPACITY INODES]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3:]...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
