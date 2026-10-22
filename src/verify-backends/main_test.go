package main

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/verify-backends/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestBackends(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Backend Verification Suite") }

var _ = Describe("Backend verification", func() {
	var client *fakes.FakeHTTPClient
	var database *fakes.FakeDatabase
	var path string
	BeforeEach(func() {
		client = &fakes.FakeHTTPClient{}
		database = &fakes.FakeDatabase{}
		client.DoStub = func(r *http.Request) (*http.Response, error) {
			status := map[string]int{"PROPFIND": 207, "PUT": 201, "GET": 200, "MKCOL": 201, "DELETE": 204}[r.Method]
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("data"))}, nil
		}
		var cfg config
		cfg.Postgres.Host = "db"
		cfg.Postgres.Port = "5432"
		cfg.Postgres.User = "user"
		cfg.Postgres.Database = "juicefs"
		cfg.Blobstore.Host = "blob"
		cfg.Blobstore.Port = "4443"
		cfg.Blobstore.User = "blob-user"
		cfg.Blobstore.Password = "blob-secret"
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		DeferCleanup(server.Close)
		cfg.Blobstore.CA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
		data, err := json.Marshal(cfg)
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(GinkgoT().TempDir(), "config.json")
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
	})
	It("checks postgres and all WebDAV operations then cleans both objects", func() {
		Expect(runWith(path, database, client)).To(Succeed())
		Expect(database.CheckCallCount()).To(Equal(1))
		Expect(client.DoCallCount()).To(Equal(6))
		methods := []string{}
		for i := 0; i < client.DoCallCount(); i++ {
			r := client.DoArgsForCall(i)
			methods = append(methods, r.Method)
			user, password, ok := r.BasicAuth()
			Expect(ok).To(BeTrue())
			Expect(user).To(Equal("blob-user"))
			Expect(password).To(Equal("blob-secret"))
		}
		Expect(methods).To(Equal([]string{"PROPFIND", "PUT", "GET", "MKCOL", "DELETE", "DELETE"}))
		Expect(client.DoArgsForCall(0).Header.Get("Depth")).To(Equal("0"))
		Expect(client.DoArgsForCall(4).URL.Path).To(HaveSuffix("-dir/"))
		Expect(client.DoArgsForCall(5).URL.Path).To(Equal(client.DoArgsForCall(1).URL.Path))
	})
	DescribeTable("cleans created objects after operation failures", func(failed string, wantCalls int) {
		original := client.DoStub
		client.DoStub = func(r *http.Request) (*http.Response, error) {
			if r.Method == failed {
				return nil, errors.New("network failed")
			}
			return original(r)
		}
		Expect(runWith(path, database, client)).To(MatchError(ContainSubstring("network failed")))
		Expect(client.DoCallCount()).To(Equal(wantCalls))
	}, Entry("discovery", "PROPFIND", 1), Entry("upload", "PUT", 2), Entry("download", "GET", 4), Entry("directory", "MKCOL", 5))
	It("stops before HTTP requests when database access fails", func() {
		database.CheckReturns(errors.New("database denied"))
		Expect(runWith(path, database, client)).To(MatchError(ContainSubstring("database denied")))
		Expect(client.DoCallCount()).To(BeZero())
	})
	It("rejects unreadable JSON and invalid CA configuration", func() {
		Expect(runWith("/nonexistent", database, client)).NotTo(Succeed())
		Expect(os.WriteFile(path, []byte("bad"), 0600)).To(Succeed())
		Expect(runWith(path, database, client)).NotTo(Succeed())
		Expect(os.WriteFile(path, []byte(`{}`), 0600)).To(Succeed())
		Expect(runWith(path, database, client)).To(MatchError("invalid blobstore CA certificate"))
	})
	It("rejects unexpected statuses and malformed URLs", func() {
		client.DoReturns(&http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied"))}, nil)
		Expect(request(client, "GET", "https://blob/admin/", "u", "p", "", 200)).To(MatchError(ContainSubstring("403")))
		Expect(request(client, "GET", "://bad", "u", "p", "", 200)).NotTo(Succeed())
	})
})
