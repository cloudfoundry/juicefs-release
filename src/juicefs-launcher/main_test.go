package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/juicefs-launcher/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLauncher(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "JuiceFS Launcher Suite") }

var _ = Describe("Launching JuiceFS", func() {
	var cfg config
	var runner *fakes.FakeRunner
	var path string
	var signals chan os.Signal
	launch := func(mode string, extra ...string) error {
		data, err := json.Marshal(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
		return runWith(mode, path, runner, signals, func() error { return nil }, extra...)
	}
	BeforeEach(func() {
		runner = &fakes.FakeRunner{}
		path = filepath.Join(GinkgoT().TempDir(), "config.json")
		signals = make(chan os.Signal, 1)
		cfg = config{}
		cfg.Metadata.Type = "postgres"
		cfg.Metadata.Host = "database.internal"
		cfg.Metadata.User = "identity"
		cfg.Metadata.Database = "juicefs"
		cfg.Metadata.Password = "db-secret"
		cfg.Storage.Type = "wasb"
		cfg.Storage.Bucket = "https://container.blob.core.windows.net"
		cfg.Storage.AccessKey = "account"
		cfg.Volume.Name = "volume"
		cfg.Volume.MountPoint = "/mount"
		cfg.Volume.CacheDir = "/cache"
	})
	DescribeTable("builds database URLs without exposing passwords", func(kind, options, want string) {
		cfg.Metadata.Type = kind
		cfg.Metadata.Options = options
		uri, err := metadataURL(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(uri).To(Equal(want))
		Expect(uri).NotTo(ContainSubstring("db-secret"))
	},
		Entry("postgres", "postgres", "sslmode=disable", "postgres://identity:@database.internal:5432/juicefs?sslmode=disable"), Entry("mysql", "mysql", "parseTime=true", "mysql://identity:@(database.internal:3306)/juicefs?parseTime=true"))
	It("formats without updating existing filesystems and passes secrets in the environment", func() {
		Expect(launch("format")).To(Succeed())
		executable, args, env, ch := runner.RunArgsForCall(0)
		Expect(executable).To(Equal("/var/vcap/packages/juicefs/bin/juicefs"))
		Expect(args).To(Equal([]string{"format", "--no-update", "--storage", "wasb", "--bucket", cfg.Storage.Bucket, "postgres://identity:@database.internal:5432/juicefs", "volume"}))
		Expect(env).To(ContainElements("META_PASSWORD=db-secret", "ACCESS_KEY=account", "SSL_CERT_FILE=/var/vcap/packages/ca-certificates/certs/ca-certificates.crt"))
		Expect(ch).To(Equal((<-chan os.Signal)(signals)))
	})
	It("mounts only the requested subdirectory in the foreground", func() {
		cfg.Volume.Subdir = "/cf-volumes/instance"
		Expect(launch("mount")).To(Succeed())
		_, args, _, _ := runner.RunArgsForCall(0)
		Expect(args).To(ContainElements("--foreground", "--subdir", "/cf-volumes/instance", "--no-usage-report", "/mount"))
	})
	It("applies quotas with all requested limits", func() {
		Expect(launch("quota", "/cf-volumes/instance", "2", "100")).To(Succeed())
		_, args, _, _ := runner.RunArgsForCall(0)
		Expect(args[:8]).To(Equal([]string{"quota", "set", "--path", "/cf-volumes/instance", "--capacity", "2", "--inodes", "100"}))
	})
	It("rejects invalid operations and escaping paths before executing", func() {
		Expect(launch("unknown")).NotTo(Succeed())
		Expect(launch("quota", "/outside", "1", "2")).NotTo(Succeed())
		Expect(launch("quota")).NotTo(Succeed())
		cfg.Volume.Subdir = "/cf-volumes/../other"
		Expect(launch("mount")).NotTo(Succeed())
		Expect(runner.RunCallCount()).To(BeZero())
	})
	It("requires the FUSE device for mounting and propagates runner errors", func() {
		Expect(launch("format")).To(Succeed())
		runner.RunReturns(errors.New("child failed"))
		Expect(launch("format")).To(MatchError("child failed"))
		Expect(runWith("mount", path, runner, signals, func() error { return errors.New("no fuse") })).To(MatchError("no fuse"))
	})
	It("rejects missing or malformed configuration and invalid backends", func() {
		Expect(runWith("format", path, runner, signals, nil)).NotTo(Succeed())
		Expect(os.WriteFile(path, []byte("bad json"), 0600)).To(Succeed())
		Expect(runWith("format", path, runner, signals, nil)).NotTo(Succeed())
		cfg.Metadata.Type = "bad"
		Expect(launch("format")).NotTo(Succeed())
		cfg.Metadata.Type = "postgres"
		cfg.Metadata.Host = ""
		Expect(launch("format")).NotTo(Succeed())
		cfg.Metadata.Host = "host"
		cfg.Storage.Type = "invalid"
		Expect(launch("format")).NotTo(Succeed())
		cfg.Storage.Type = "wasb"
		cfg.Storage.Bucket = "file:///bucket"
		Expect(launch("format")).NotTo(Succeed())
	})
	It("supports legacy postgres and WebDAV configuration and custom trust", func() {
		cfg.Metadata.Type = ""
		cfg.Postgres.Host = "legacy"
		cfg.Postgres.User = "user"
		cfg.Postgres.Database = "db"
		cfg.Postgres.Password = "legacy-secret"
		cfg.Postgres.Port = "5432"
		cfg.Storage.Type = ""
		cfg.Blobstore.Hostname = "blob"
		cfg.Blobstore.Port = "4443"
		cfg.Blobstore.User = "user"
		cfg.Blobstore.Password = "blob-secret"
		cfg.Blobstore.CAFile = "/legacy-ca"
		Expect(launch("format")).To(Succeed())
		_, _, env, _ := runner.RunArgsForCall(0)
		Expect(env).To(ContainElements("META_PASSWORD=legacy-secret", "SECRET_KEY=blob-secret", "SSL_CERT_FILE=/legacy-ca"))
		cfg.Storage.Type = "webdav"
		cfg.Storage.Bucket = "https://blob/admin/"
		cfg.Storage.CACert = "pem"
		cfg.Storage.CAFile = "/custom-ca"
		Expect(launch("format")).To(Succeed())
		_, _, env, _ = runner.RunArgsForCall(1)
		Expect(env).To(ContainElement("SSL_CERT_FILE=/custom-ca"))
	})
	It("restricts credentials to the selected managed identity", func() {
		cfg.Storage.ManagedIdentity = true
		env, err := storageEnvironment(cfg, []string{"PATH=/bin", "AZURE_CLIENT_ID=wrong", "AZURE_TOKEN_CREDENTIALS=EnvironmentCredential"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(ConsistOf("PATH=/bin", "AZURE_TOKEN_CREDENTIALS=ManagedIdentityCredential"))
		cfg.Storage.ManagedIdentityClientID = "selected"
		env, err = storageEnvironment(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(ContainElement("AZURE_CLIENT_ID=selected"))
		_, err = storageEnvironment(cfg, []string{"AZURE_STORAGE_CONNECTION_STRING=secret"})
		Expect(err).To(HaveOccurred())
		Expect(launch("format")).To(Succeed())
		_, _, env, _ = runner.RunArgsForCall(0)
		Expect(env).To(ContainElement("AZURE_CLIENT_ID=selected"))
	})
	DescribeTable("rejects conflicting managed identity settings", func(change func(*config)) {
		cfg.Storage.ManagedIdentity = true
		change(&cfg)
		_, err := storageEnvironment(cfg, nil)
		Expect(err).To(HaveOccurred())
	},
		Entry("wrong backend", func(c *config) { c.Storage.Type = "s3" }), Entry("insecure endpoint", func(c *config) { c.Storage.Bucket = "http://blob" }), Entry("account key", func(c *config) { c.Storage.SecretKey = "key" }), Entry("SAS", func(c *config) { c.Storage.SessionToken = "sas" }), Entry("missing account", func(c *config) { c.Storage.AccessKey = "" }), Entry("disabled", func(c *config) { c.Storage.ManagedIdentity = false; c.Storage.ManagedIdentityClientID = "id" }))
	It("rejects reserved MySQL characters", func() {
		cfg.Metadata.Type = "mysql"
		cfg.Metadata.User = "bad@user"
		_, err := metadataURL(cfg)
		Expect(err).To(HaveOccurred())
	})
})
