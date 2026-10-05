// juicefs-service runs the JuiceFS CLI with a loopback WebDAV endpoint. The
// upstream WebDAV client cannot validate the CN-only certificate issued by the
// POC director, so the proxy pins the exact server certificate from the link.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	Postgres struct {
		Host, Port, Database, User, Password string
	} `json:"postgres"`
	Blobstore struct {
		Host, Port, User, Password, Certificate string
	} `json:"blobstore"`
	Volume struct {
		Name, MountPoint, CacheDir, Subdir string
	} `json:"volume"`
}

const webDAVProxyAddress = "127.0.0.1:19798"

func serveProxy(cfg config) (*http.Server, net.Listener, error) {
	block, _ := pem.Decode([]byte(cfg.Blobstore.Certificate))
	if block == nil {
		return nil, nil, errors.New("invalid linked WebDAV certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.Blobstore.Certificate)) {
		return nil, nil, errors.New("invalid linked WebDAV trust root")
	}
	upstream, err := url.Parse(fmt.Sprintf("https://%s:%s", cfg.Blobstore.Host, cfg.Blobstore.Port))
	if err != nil {
		return nil, nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = &http.Transport{TLSClientConfig: &tls.Config{
		// This director issues a legacy CN-only cert. Verify the chain and pin
		// the exact certificate provided by the authenticated BOSH link.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, block.Bytes) {
				return errors.New("WebDAV certificate differs from BOSH link")
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}}
	listenConfig := net.ListenConfig{Control: func(_, _ string, rawConn syscall.RawConn) error {
		var socketErr error
		if err := rawConn.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return socketErr
	}}
	listener, err := listenConfig.Listen(context.Background(), "tcp", webDAVProxyAddress)
	if err != nil {
		return nil, nil, err
	}
	server := &http.Server{Handler: proxy}
	go server.Serve(listener)
	return server, listener, nil
}

func waitForProxy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", webDAVProxyAddress, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for cell WebDAV proxy")
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
	var server *http.Server
	var listener net.Listener
	if mode == "proxy" || mode == "format" || (mode == "mount" && cfg.Volume.Subdir == "") {
		server, listener, err = serveProxy(cfg)
		if err != nil {
			return err
		}
		defer server.Close()
		defer listener.Close()
	}
	if mode == "mount" && cfg.Volume.Subdir != "" {
		if !strings.HasPrefix(cfg.Volume.Subdir, "/cf-volumes/") || strings.Contains(cfg.Volume.Subdir, "..") {
			return fmt.Errorf("invalid JuiceFS subdirectory %q", cfg.Volume.Subdir)
		}
		if err := waitForProxy(5 * time.Second); err != nil {
			return err
		}
	}
	if mode == "proxy" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(signals)
		<-signals
		return nil
	}

	metaURL := fmt.Sprintf("postgres://%s:@%s:%s/%s?sslmode=disable", cfg.Postgres.User, cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database)
	bucket := fmt.Sprintf("http://%s/admin/", webDAVProxyAddress)
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
	if len(os.Args) != 3 || (os.Args[1] != "format" && os.Args[1] != "mount" && os.Args[1] != "proxy") {
		fmt.Fprintln(os.Stderr, "usage: juicefs-service format|mount|proxy CONFIG.json")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
