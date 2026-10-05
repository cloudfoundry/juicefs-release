// juicefs-service runs the JuiceFS CLI with a BOSH DNS WebDAV endpoint and
// the CA trust supplied by its backend link.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

type config struct {
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

func run(mode, configPath string) error {
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

	metaURL := fmt.Sprintf("postgres://%s:@%s:%s/%s?sslmode=disable", cfg.Postgres.User, cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database)
	if cfg.Blobstore.Hostname == "" || cfg.Blobstore.CAFile == "" {
		return fmt.Errorf("WebDAV hostname and CA file are required")
	}
	bucket := fmt.Sprintf("https://%s:%s/admin/", cfg.Blobstore.Hostname, cfg.Blobstore.Port)
	var args []string
	switch mode {
	case "format":
		args = []string{"format", "--no-update", "--storage", "webdav", "--bucket", bucket, metaURL, cfg.Volume.Name}
	case "mount":
		args = []string{"mount", "--foreground", "--subdir", cfg.Volume.Subdir, "--cache-dir", cfg.Volume.CacheDir, "--cache-size", "1024", "--backup-meta", "0", "--no-usage-report", metaURL, cfg.Volume.MountPoint}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	cmd := exec.Command("/var/vcap/packages/juicefs/bin/juicefs", args...)
	cmd.Env = append(os.Environ(), "META_PASSWORD="+cfg.Postgres.Password, "ACCESS_KEY="+cfg.Blobstore.User, "SECRET_KEY="+cfg.Blobstore.Password)
	if cfg.Blobstore.CAFile != "" {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+cfg.Blobstore.CAFile)
	}
	if mode == "format" {
		cmd.Env = append(cmd.Env, "JFS_BLOB="+bucket)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if mode == "mount" {
		fuse, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open /dev/fuse: %w", err)
		}
		fuse.Close()
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		return err
	case sig := <-signals:
		_ = cmd.Process.Signal(sig)
		return <-done
	}
}

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "format" && os.Args[1] != "mount") {
		fmt.Fprintln(os.Stderr, "usage: juicefs-service format|mount CONFIG.json")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
