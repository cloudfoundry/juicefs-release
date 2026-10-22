package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudfoundry/go-cfclient/v3/resource"
	"github.com/cloudfoundry/juicefs-release/src/broker-registration/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRegistration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Broker Registration Suite")
}

var _ = Describe("Broker registration", func() {
	var api *fakes.FakeAPI
	var cfg config
	var ctx context.Context
	broker := func(guid string) *resource.ServiceBroker {
		return &resource.ServiceBroker{Resource: resource.Resource{GUID: guid}}
	}
	BeforeEach(func() {
		ctx = context.Background()
		api = &fakes.FakeAPI{}
		cfg = config{APIURL: "api.example.test", CFUsername: "admin", CFPassword: "admin-secret", BrokerName: "juicefs", BrokerURL: "http://broker", BrokerUsername: "user", BrokerPassword: "secret"}
		api.BrokersReturns([]*resource.ServiceBroker{broker("broker-guid")}, nil)
		api.OfferingsReturns([]*resource.ServiceOffering{{Resource: resource.Resource{GUID: "offering-guid"}}}, nil)
		api.PlansReturns([]*resource.ServicePlan{{Resource: resource.Resource{GUID: "shared-guid"}}, {Resource: resource.Resource{GUID: "small-guid"}}}, nil)
		for _, key := range []string{"CF_USERNAME", "CF_PASSWORD", "BROKER_USERNAME", "BROKER_PASSWORD", "CF_API_URL", "CF_CA_CERT", "BROKER_TLS_SKIP"} {
			GinkgoT().Setenv(key, "")
		}
	})
	It("creates a broker and enables every plan after catalog import", func() {
		api.BrokersReturnsOnCall(0, nil, nil)
		Expect(runRegistration(ctx, "register", cfg, api)).To(Succeed())
		_, request := api.CreateBrokerArgsForCall(0)
		Expect(request).To(Equal(resource.NewServiceBrokerCreate("juicefs", "http://broker", "user", "secret")))
		Expect(api.UpdateBrokerCallCount()).To(BeZero())
		Expect(api.BrokersCallCount()).To(Equal(2))
		_, guid := api.OfferingsArgsForCall(0)
		Expect(guid).To(Equal("broker-guid"))
		_, guid = api.PlansArgsForCall(0)
		Expect(guid).To(Equal("offering-guid"))
		Expect(api.EnablePlanCallCount()).To(Equal(2))
		_, guid = api.EnablePlanArgsForCall(1)
		Expect(guid).To(Equal("small-guid"))
	})
	It("updates the existing broker instead of creating another", func() {
		Expect(runRegistration(ctx, "register", cfg, api)).To(Succeed())
		Expect(api.CreateBrokerCallCount()).To(BeZero())
		_, guid, request := api.UpdateBrokerArgsForCall(0)
		Expect(guid).To(Equal("broker-guid"))
		Expect(*request.URL).To(Equal("http://broker"))
		Expect(request.Authentication.Credentials.Password).To(Equal("secret"))
	})
	It("propagates discovery, catalog, plan and visibility errors", func() {
		api.BrokersReturns(nil, errors.New("discovery denied"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("discovery denied")))
		api.BrokersReturns([]*resource.ServiceBroker{broker("broker-guid")}, nil)
		api.OfferingsReturns(nil, errors.New("offering denied"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("offering denied")))
		api.OfferingsReturns([]*resource.ServiceOffering{{Resource: resource.Resource{GUID: "offering-guid"}}}, nil)
		api.PlansReturns(nil, errors.New("plans denied"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("plans denied")))
		api.PlansReturns([]*resource.ServicePlan{{Resource: resource.Resource{GUID: "plan"}}}, nil)
		api.EnablePlanReturns(errors.New("enable denied"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("enable denied")))
	})
	It("stops when create or update jobs fail", func() {
		api.UpdateBrokerReturns(errors.New("update failed"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("update failed")))
		Expect(api.OfferingsCallCount()).To(BeZero())
		api.BrokersReturns(nil, nil)
		api.CreateBrokerReturns(errors.New("create failed"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("create failed")))
	})
	It("reports rediscovery failures after a successful import", func() {
		api.BrokersReturnsOnCall(1, nil, errors.New("rediscovery denied"))
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("rediscovery denied")))
		Expect(api.OfferingsCallCount()).To(BeZero())
	})
	It("rejects ambiguous brokers and missing imported resources", func() {
		api.BrokersReturns([]*resource.ServiceBroker{broker("a"), broker("b")}, nil)
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("multiple")))
		api.BrokersReturns(nil, nil)
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("not found")))
		api.BrokersReturns([]*resource.ServiceBroker{broker("broker-guid")}, nil)
		api.OfferingsReturns(nil, nil)
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("one JuiceFS")))
		api.OfferingsReturns([]*resource.ServiceOffering{{Resource: resource.Resource{GUID: "offering-guid"}}}, nil)
		api.PlansReturns(nil, nil)
		Expect(runRegistration(ctx, "register", cfg, api)).To(MatchError(ContainSubstring("no JuiceFS service plans")))
	})
	It("removes plans and offering before the broker without deleting instances", func() {
		order := []string{}
		api.DeletePlanStub = func(_ context.Context, guid string) error { order = append(order, guid); return nil }
		api.DeleteOfferingStub = func(_ context.Context, guid string) error { order = append(order, guid); return nil }
		api.DeleteBrokerStub = func(_ context.Context, guid string) error { order = append(order, guid); return nil }
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(Succeed())
		Expect(order).To(Equal([]string{"shared-guid", "small-guid", "offering-guid", "broker-guid"}))
		Expect(api.EnablePlanCallCount()).To(BeZero())
	})
	It("treats an absent broker or catalog as an idempotent deregistration", func() {
		api.BrokersReturns(nil, nil)
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(Succeed())
		Expect(api.DeleteBrokerCallCount()).To(BeZero())
		api.BrokersReturns([]*resource.ServiceBroker{broker("broker-guid")}, nil)
		api.OfferingsReturns(nil, nil)
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(Succeed())
		Expect(api.DeleteBrokerCallCount()).To(Equal(1))
	})
	It("does not bypass an in-use plan or other catalog deletion failures", func() {
		api.DeletePlanReturns(errors.New("plan in use"))
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(MatchError(ContainSubstring("plan in use")))
		Expect(api.DeleteOfferingCallCount()).To(BeZero())
		Expect(api.DeleteBrokerCallCount()).To(BeZero())
		api.DeletePlanReturns(nil)
		api.DeleteOfferingReturns(errors.New("offering denied"))
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(MatchError(ContainSubstring("offering denied")))
		Expect(api.DeleteBrokerCallCount()).To(BeZero())
		api.DeleteOfferingReturns(nil)
		api.DeleteBrokerReturns(errors.New("broker denied"))
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(MatchError(ContainSubstring("broker denied")))
	})
	It("propagates deregistration listing failures", func() {
		api.OfferingsReturns(nil, errors.New("unavailable"))
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(MatchError(ContainSubstring("unavailable")))
		api.OfferingsReturns([]*resource.ServiceOffering{{Resource: resource.Resource{GUID: "offering"}}}, nil)
		api.PlansReturns(nil, errors.New("plans unavailable"))
		Expect(runRegistration(ctx, "deregister", cfg, api)).To(MatchError(ContainSubstring("plans unavailable")))
	})
	It("rejects unsupported modes before accessing CF", func() {
		Expect(runRegistration(ctx, "wrong", cfg, api)).To(MatchError("invalid registration mode"))
		Expect(api.BrokersCallCount()).To(BeZero())
	})
	It("applies environment overrides without creating CLI state", func() {
		GinkgoT().Setenv("CF_USERNAME", "override")
		GinkgoT().Setenv("CF_PASSWORD", "new-secret")
		GinkgoT().Setenv("CF_API_URL", "https://api.override")
		GinkgoT().Setenv("CF_CA_CERT", "pem")
		GinkgoT().Setenv("BROKER_USERNAME", "broker-user")
		GinkgoT().Setenv("BROKER_PASSWORD", "broker-secret")
		cfg.BrokerURL = ""
		cfg.BrokerPort = 8080
		prepared, err := prepareConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(prepared.CFUsername).To(Equal("override"))
		Expect(prepared.CFPassword).To(Equal("new-secret"))
		Expect(prepared.APIURL).To(Equal("https://api.override"))
		Expect(prepared.CACert).To(Equal("pem"))
		Expect(prepared.BrokerUsername).To(Equal("broker-user"))
		Expect(prepared.BrokerPassword).To(Equal("broker-secret"))
		Expect(prepared.BrokerURL).To(Equal("http://127.0.0.1:8080"))
	})
	It("validates credentials and rejects misleading broker TLS overrides", func() {
		cfg.CFPassword = ""
		_, err := prepareConfig(cfg)
		Expect(err).To(HaveOccurred())
		cfg.CFPassword = "secret"
		cfg.BrokerURL = ""
		_, err = prepareConfig(cfg)
		Expect(err).To(HaveOccurred())
		cfg.BrokerURL = "https://broker"
		GinkgoT().Setenv("BROKER_TLS_SKIP", "true")
		_, err = prepareConfig(cfg)
		Expect(err).To(MatchError(ContainSubstring("performed by Cloud Controller")))
	})
	It("reports unreadable and malformed configuration", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.json")
		Expect(run("register", path)).To(HaveOccurred())
		Expect(os.WriteFile(path, []byte("broken"), 0600)).To(Succeed())
		Expect(run("register", path)).To(HaveOccurred())
		Expect(os.WriteFile(path, []byte(`{}`), 0600)).To(Succeed())
		Expect(run("register", path)).To(HaveOccurred())
	})
	DescribeTable("normalizes API URLs", func(input, want string) { Expect(normalizeAPIURL(input)).To(Equal(want)) }, Entry("bare", "api.example", "https://api.example"), Entry("https", "https://api.example", "https://api.example"), Entry("http", "http://api.example", "http://api.example"))
	It("uses fallback for an empty environment variable", func() { GinkgoT().Setenv("EMPTY", ""); Expect(envOr("EMPTY", "fallback")).To(Equal("fallback")) })
})
