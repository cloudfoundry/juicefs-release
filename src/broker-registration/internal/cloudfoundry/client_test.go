package cloudfoundry_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cloudfoundry/go-cfclient/v3/resource"
	"github.com/cloudfoundry/juicefs-release/src/broker-registration/internal/cloudfoundry"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestClient(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Go CF Client Adapter Suite") }

var _ = Describe("go-cfclient adapter", func() {
	var server *httptest.Server
	var mu sync.Mutex
	var requests []string
	var jobState string
	var failPath string
	var tokenDenied bool
	BeforeEach(func() {
		requests = nil
		jobState = "COMPLETE"
		failPath = ""
		tokenDenied = false
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/" {
				fmt.Fprintf(w, `{"links":{"uaa":{"href":%q},"login":{"href":%q}}}`, server.URL, server.URL)
				return
			}
			if r.URL.Path == "/oauth/token" {
				if tokenDenied {
					w.WriteHeader(401)
					fmt.Fprint(w, `{"error":"invalid_grant"}`)
					return
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "password" || r.Form.Get("username") != "admin" || r.Form.Get("password") != "secret" {
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{"access_token":"token","token_type":"bearer","expires_in":3600}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer token" {
				w.WriteHeader(401)
				return
			}
			if r.URL.Path == failPath {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"errors":[{"title":"CF-NotAuthorized","detail":"denied","code":10003}]}`)
				return
			}
			switch {
			case r.URL.Path == "/v3/service_brokers" && r.Method == "GET":
				if r.URL.Query().Get("names") != "juicefs" {
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{"pagination":{"total_pages":1},"resources":[{"guid":"broker","name":"juicefs"}]}`)
			case (r.URL.Path == "/v3/service_brokers" && r.Method == "POST") || (r.URL.Path == "/v3/service_brokers/broker" && (r.Method == "PATCH" || r.Method == "DELETE")):
				if r.Method != "DELETE" {
					var payload map[string]any
					if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["url"] != "https://broker" {
						w.WriteHeader(400)
						return
					}
				}
				w.Header().Set("Location", server.URL+"/v3/jobs/job")
				w.WriteHeader(202)
			case r.URL.Path == "/v3/jobs/job":
				fmt.Fprintf(w, `{"guid":"job","state":%q,"errors":[{"detail":"catalog rejected"}]}`, jobState)
			case r.URL.Path == "/v3/service_offerings" && r.Method == "GET":
				if r.URL.Query().Get("service_broker_guids") != "broker" || r.URL.Query().Get("names") != "juicefs" {
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{"pagination":{"total_pages":1},"resources":[{"guid":"offering"}]}`)
			case r.URL.Path == "/v3/service_plans" && r.Method == "GET":
				if r.URL.Query().Get("service_offering_guids") != "offering" {
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{"pagination":{"total_pages":1},"resources":[{"guid":"plan"}]}`)
			case r.URL.Path == "/v3/service_plans/plan/visibility":
				var payload resource.ServicePlanVisibility
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload.Type != "public" {
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{"type":"public"}`)
			case r.Method == "DELETE" && (r.URL.Path == "/v3/service_plans/plan" || r.URL.Path == "/v3/service_offerings/offering"):
				w.WriteHeader(204)
			default:
				w.WriteHeader(404)
			}
		}))
		DeferCleanup(server.Close)
	})
	newClient := func() (*cloudfoundry.Client, error) {
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		return cloudfoundry.New(server.URL, "admin", "secret", string(ca), false)
	}
	It("uses password authentication, custom CA trust, typed APIs and asynchronous jobs", func() {
		api, err := newClient()
		Expect(err).NotTo(HaveOccurred())
		ctx := context.Background()
		brokers, err := api.Brokers(ctx, "juicefs")
		Expect(err).NotTo(HaveOccurred())
		Expect(brokers[0].GUID).To(Equal("broker"))
		Expect(api.CreateBroker(ctx, resource.NewServiceBrokerCreate("juicefs", "https://broker", "user", "password"))).To(Succeed())
		Expect(api.UpdateBroker(ctx, "broker", resource.NewServiceBrokerUpdate().WithURL("https://broker").WithCredentials("user", "password"))).To(Succeed())
		offerings, err := api.Offerings(ctx, "broker")
		Expect(err).NotTo(HaveOccurred())
		Expect(offerings[0].GUID).To(Equal("offering"))
		plans, err := api.Plans(ctx, "offering")
		Expect(err).NotTo(HaveOccurred())
		Expect(plans[0].GUID).To(Equal("plan"))
		Expect(api.EnablePlan(ctx, "plan")).To(Succeed())
		Expect(api.DeletePlan(ctx, "plan")).To(Succeed())
		Expect(api.DeleteOffering(ctx, "offering")).To(Succeed())
		Expect(api.DeleteBroker(ctx, "broker")).To(Succeed())
		mu.Lock()
		defer mu.Unlock()
		Expect(requests).To(ContainElement("GET /v3/jobs/job"))
	})
	It("rejects untrusted certificates, malformed custom roots and bad credentials", func() {
		_, err := cloudfoundry.New(server.URL, "admin", "secret", "", false)
		Expect(err).To(HaveOccurred())
		_, err = cloudfoundry.New(server.URL, "admin", "secret", "bad pem", false)
		Expect(err).To(MatchError("invalid Cloud Controller CA certificate"))
		mu.Lock()
		tokenDenied = true
		mu.Unlock()
		_, err = newClient()
		Expect(err).To(HaveOccurred())
	})
	It("honors explicit TLS skip and propagates CAPI request failures", func() {
		api, err := cloudfoundry.New(server.URL, "admin", "secret", "", true)
		Expect(err).NotTo(HaveOccurred())
		mu.Lock()
		failPath = "/v3/service_plans/plan"
		mu.Unlock()
		Expect(api.DeletePlan(context.Background(), "plan")).To(MatchError(ContainSubstring("denied")))
	})
	It("fails on asynchronous broker job errors", func() {
		api, err := newClient()
		Expect(err).NotTo(HaveOccurred())
		mu.Lock()
		jobState = "FAILED"
		mu.Unlock()
		Expect(api.DeleteBroker(context.Background(), "broker")).To(MatchError(ContainSubstring("catalog rejected")))
	})
	It("propagates asynchronous request failures without polling a nonexistent job", func() {
		api, err := newClient()
		Expect(err).NotTo(HaveOccurred())
		mu.Lock()
		failPath = "/v3/service_brokers"
		mu.Unlock()
		Expect(api.CreateBroker(context.Background(), resource.NewServiceBrokerCreate("juicefs", "https://broker", "user", "password"))).To(MatchError(ContainSubstring("denied")))
		mu.Lock()
		defer mu.Unlock()
		Expect(requests).NotTo(ContainElement("GET /v3/jobs/job"))
	})
})
