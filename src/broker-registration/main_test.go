package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/broker-registration/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRegistration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Broker Registration Suite")
}

var _ = Describe("Broker registration", func() {
	var runner *fakes.FakeRunner
	var cfg config
	BeforeEach(func() {
		runner = &fakes.FakeRunner{}
		cfg = config{BrokerName: "juicefs", BrokerUsername: "user", BrokerPassword: "secret", BrokerURL: "http://broker"}
	})
	Describe("registration orchestration", func() {
		BeforeEach(func() {
			cfg.APIURL = "api.example.test"
			cfg.CFUsername = "admin"
			cfg.CFPassword = "admin-secret"
			for _, key := range []string{"CF_USERNAME", "CF_PASSWORD", "BROKER_USERNAME", "BROKER_PASSWORD", "CF_API_URL", "CF_CA_CERT", "BROKER_TLS_SKIP"} {
				GinkgoT().Setenv(key, "")
			}
		})
		It("configures API, authenticates, registers, enables access and logs out", func() {
			runner.OutputReturnsOnCall(2, []byte(`{"pagination":{"total_results":0}}`), nil)
			runner.OutputReturnsOnCall(4, []byte(`{"resources":[{"guid":"offering"}]}`), nil)
			cfg.SkipSSLValidation = true
			cfg.CACert = "test-ca"
			cfg.BrokerURL = ""
			cfg.BrokerPort = 8080
			dir := GinkgoT().TempDir()
			Expect(runRegistration("register", cfg, dir, runner)).To(Succeed())
			env, args := runner.OutputArgsForCall(0)
			Expect(args).To(Equal([]string{"api", "https://api.example.test", "--skip-ssl-validation"}))
			Expect(env).To(ContainElements("CF_USERNAME=admin", "CF_PASSWORD=admin-secret", "SSL_CERT_FILE="+filepath.Join(dir, "cloud-controller-ca.crt")))
			info, err := os.Stat(filepath.Join(dir, "cloud-controller-ca.crt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
			_, args = runner.OutputArgsForCall(3)
			Expect(args[4]).To(Equal("http://127.0.0.1:8080"))
			_, args = runner.OutputArgsForCall(6)
			Expect(args).To(Equal([]string{"logout"}))
		})
		It("deletes registration even if the offering was already absent", func() {
			runner.OutputReturnsOnCall(2, nil, errors.New("already absent"))
			Expect(runRegistration("deregister", cfg, GinkgoT().TempDir(), runner)).To(Succeed())
			_, args := runner.OutputArgsForCall(3)
			Expect(args).To(Equal([]string{"delete-service-broker", "juicefs", "-f"}))
		})
		DescribeTable("propagates CLI failures at every stage", func(call int) {
			runner.OutputReturnsOnCall(2, []byte(`{"pagination":{"total_results":0}}`), nil)
			runner.OutputReturnsOnCall(4, []byte(`{"resources":[{"guid":"offering"}]}`), nil)
			runner.OutputReturnsOnCall(call, nil, errors.New("denied"))
			Expect(runRegistration("register", cfg, GinkgoT().TempDir(), runner)).To(MatchError(ContainSubstring("denied")))
		}, Entry("api", 0), Entry("auth", 1), Entry("discovery", 2), Entry("create", 3), Entry("offering", 4), Entry("enable", 5))
		It("rejects invalid mode, missing credentials, missing endpoint, and unsupported TLS skip", func() {
			Expect(runRegistration("wrong", cfg, GinkgoT().TempDir(), runner)).NotTo(Succeed())
			cfg.CFPassword = ""
			Expect(runRegistration("register", cfg, GinkgoT().TempDir(), runner)).NotTo(Succeed())
			cfg.CFPassword = "secret"
			cfg.BrokerURL = ""
			Expect(runRegistration("register", cfg, GinkgoT().TempDir(), runner)).NotTo(Succeed())
			cfg.BrokerURL = "https://broker"
			cfg.SkipBrokerSSLValidation = true
			Expect(runRegistration("register", cfg, GinkgoT().TempDir(), runner)).NotTo(Succeed())
			Expect(runner.OutputCallCount()).To(BeZero())
		})
		It("propagates deregistration failures", func() {
			runner.OutputReturnsOnCall(3, nil, errors.New("denied"))
			Expect(runRegistration("deregister", cfg, GinkgoT().TempDir(), runner)).To(MatchError(ContainSubstring("denied")))
		})
	})
	DescribeTable("normalizes API URLs", func(input, want string) { Expect(normalizeAPIURL(input)).To(Equal(want)) },
		Entry("bare host", "api.example.test", "https://api.example.test"), Entry("https", "https://api.example.test", "https://api.example.test"), Entry("http", "http://api.example.test", "http://api.example.test"))
	It("uses nonempty environment overrides", func() {
		GinkgoT().Setenv("REGISTRATION_TEST", "override")
		Expect(envOr("REGISTRATION_TEST", "fallback")).To(Equal("override"))
		GinkgoT().Setenv("REGISTRATION_TEST", "")
		Expect(envOr("REGISTRATION_TEST", "fallback")).To(Equal("fallback"))
	})
	DescribeTable("creates or updates the broker", func(total int, action string) {
		if total == 0 {
			runner.OutputReturnsOnCall(0, []byte(`{"pagination":{"total_results":0}}`), nil)
		} else {
			runner.OutputReturnsOnCall(0, []byte(`{"pagination":{"total_results":1}}`), nil)
		}
		Expect(registerBrokerWith(runner, []string{"CF_HOME=/private"}, cfg)).To(Succeed())
		env, args := runner.OutputArgsForCall(1)
		Expect(env).To(Equal([]string{"CF_HOME=/private"}))
		Expect(args).To(Equal([]string{action, "juicefs", "user", "secret", "http://broker"}))
	}, Entry("new", 0, "create-service-broker"), Entry("existing", 1, "update-service-broker"))
	It("propagates discovery, parse, and registration failures", func() {
		runner.OutputReturns(nil, errors.New("unavailable"))
		Expect(registerBrokerWith(runner, nil, cfg)).To(MatchError("unavailable"))
		runner.OutputReturns([]byte(`broken`), nil)
		Expect(registerBrokerWith(runner, nil, cfg)).NotTo(Succeed())
		runner.OutputReturnsOnCall(2, []byte(`{"pagination":{"total_results":0}}`), nil)
		runner.OutputReturnsOnCall(3, nil, errors.New("denied"))
		Expect(registerBrokerWith(runner, nil, cfg)).To(MatchError("denied"))
	})
	It("enables access only after discovering one offering", func() {
		runner.OutputReturnsOnCall(0, []byte(`{"resources":[{"guid":"offering"}]}`), nil)
		Expect(enableServiceAccessWith(runner, nil, "juicefs")).To(Succeed())
		_, args := runner.OutputArgsForCall(1)
		Expect(args).To(Equal([]string{"enable-service-access", "-b", "juicefs", "juicefs"}))
	})
	DescribeTable("rejects missing, ambiguous and malformed offerings", func(output string) {
		runner.OutputReturns([]byte(output), nil)
		Expect(enableServiceAccessWith(runner, nil, "juicefs")).NotTo(Succeed())
		Expect(runner.OutputCallCount()).To(Equal(1))
	},
		Entry("missing", `{"resources":[]}`), Entry("empty guid", `{"resources":[{}]}`), Entry("ambiguous", `{"resources":[{"guid":"a"},{"guid":"b"}]}`), Entry("invalid JSON", `broken`))
	It("propagates offering discovery and enablement errors", func() {
		runner.OutputReturns(nil, errors.New("unavailable"))
		Expect(enableServiceAccessWith(runner, nil, "juicefs")).To(MatchError("unavailable"))
		runner.OutputReturnsOnCall(1, []byte(`{"resources":[{"guid":"a"}]}`), nil)
		runner.OutputReturnsOnCall(2, nil, errors.New("denied"))
		Expect(enableServiceAccessWith(runner, nil, "juicefs")).To(MatchError("denied"))
	})
})
