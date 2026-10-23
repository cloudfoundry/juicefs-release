package acceptance_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudfoundry/go-cfclient/v3/client"
	"github.com/cloudfoundry/go-cfclient/v3/config"
	"github.com/cloudfoundry/go-cfclient/v3/resource"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type settings struct {
	APIURL            string `json:"apiURL"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	CACert            string `json:"caCert"`
	AppsDomain        string `json:"appsDomain"`
	BrokerName        string `json:"brokerName"`
	PlanName          string `json:"planName"`
	SkipSSLValidation bool   `json:"skipSSLValidation"`
	AppBinary         string `json:"appBinary"`
}

func TestAcceptance(t *testing.T) {
	if os.Getenv("JUICEFS_ACCEPTANCE_CONFIG") == "" {
		t.Skip("set JUICEFS_ACCEPTANCE_CONFIG to run against Cloud Foundry")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "JuiceFS CF persistence acceptance")
}

var _ = Describe("a bound JuiceFS volume", func() {
	It("preserves a posted GUID after CAPI restarts the application", func(ctx SpecContext) {
		var cfg settings
		contents, err := os.ReadFile(os.Getenv("JUICEFS_ACCEPTANCE_CONFIG"))
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(contents, &cfg)).To(Succeed())
		Expect(cfg.AppsDomain).NotTo(BeEmpty())
		roots, err := x509.SystemCertPool()
		Expect(err).NotTo(HaveOccurred())
		if cfg.CACert != "" {
			Expect(roots.AppendCertsFromPEM([]byte(cfg.CACert))).To(BeTrue())
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.SkipSSLValidation}
		DeferCleanup(transport.CloseIdleConnections)
		httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
		options := []config.Option{config.UserPassword(cfg.Username, cfg.Password), config.HttpClient(httpClient), config.RequestTimeout(30 * time.Second)}
		if cfg.SkipSSLValidation {
			options = append(options, config.SkipTLSValidation())
		}
		cfConfig, err := config.New(cfg.APIURL, options...)
		Expect(err).NotTo(HaveOccurred())
		cf, err := client.New(cfConfig)
		Expect(err).NotTo(HaveOccurred())
		poll := &client.PollingOptions{Timeout: 5 * time.Minute, CheckInterval: time.Second, FailedState: "FAILED"}
		wait := func(ctx context.Context, job string, err error) {
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			if job != "" {
				ExpectWithOffset(1, cf.Jobs.PollComplete(ctx, job, poll)).To(Succeed())
			}
		}
		// Independent cleanup contexts still run when the spec's deadline expires.
		cleanup := func(name string, remove func(context.Context) (string, error)) {
			DeferCleanup(func() {
				By("deleting " + name)
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()
				job, err := remove(cleanupCtx)
				wait(cleanupCtx, job, err)
			})
		}

		By("finding the plan scoped to the registered broker")
		brokers := client.NewServiceBrokerListOptions()
		brokers.Names.EqualTo(cfg.BrokerName)
		broker, err := cf.ServiceBrokers.Single(ctx, brokers)
		Expect(err).NotTo(HaveOccurred())
		offerings := client.NewServiceOfferingListOptions()
		offerings.ServiceBrokerGUIDs.EqualTo(broker.GUID)
		offerings.Names.EqualTo("juicefs")
		offering, err := cf.ServiceOfferings.Single(ctx, offerings)
		Expect(err).NotTo(HaveOccurred())
		plans := client.NewServicePlanListOptions()
		plans.ServiceOfferingGUIDs.EqualTo(offering.GUID)
		plans.Names.EqualTo(cfg.PlanName)
		plan, err := cf.ServicePlans.Single(ctx, plans)
		Expect(err).NotTo(HaveOccurred())

		name := "juicefs-at-" + strings.ToLower(rand.Text()[:12])
		By("creating isolated org and space " + name)
		org, err := cf.Organizations.Create(ctx, resource.NewOrganizationCreate(name))
		Expect(err).NotTo(HaveOccurred())
		cleanup("organization", func(ctx context.Context) (string, error) { return cf.Organizations.Delete(ctx, org.GUID) })
		space, err := cf.Spaces.Create(ctx, resource.NewSpaceCreate(name, org.GUID))
		Expect(err).NotTo(HaveOccurred())
		cleanup("space", func(ctx context.Context) (string, error) { return cf.Spaces.Delete(ctx, space.GUID) })

		By("provisioning a managed JuiceFS service instance")
		job, err := cf.ServiceInstances.CreateManaged(ctx, resource.NewServiceInstanceCreateManaged(name, space.GUID, plan.GUID))
		wait(ctx, job, err)
		instances := client.NewServiceInstanceListOptions()
		instances.Names.EqualTo(name)
		instances.SpaceGUIDs.EqualTo(space.GUID)
		instance, err := cf.ServiceInstances.Single(ctx, instances)
		Expect(err).NotTo(HaveOccurred())
		cleanup("service instance", func(ctx context.Context) (string, error) { return cf.ServiceInstances.Delete(ctx, instance.GUID) })

		By("creating the app and binding the service at /data")
		token := rand.Text()
		appRequest := resource.NewAppCreate(name, space.GUID)
		appRequest.Lifecycle = &resource.Lifecycle{Type: "buildpack", Data: resource.BuildpackLifecycle{Buildpacks: []string{"binary_buildpack"}, Stack: "cflinuxfs4"}}
		appRequest.EnvironmentVariables = map[string]string{"TEST_TOKEN": token}
		app, err := cf.Applications.Create(ctx, appRequest)
		Expect(err).NotTo(HaveOccurred())
		cleanup("application and its binding", func(ctx context.Context) (string, error) { return cf.Applications.Delete(ctx, app.GUID) })
		job, _, err = cf.ServiceCredentialBindings.Create(ctx, resource.NewServiceCredentialBindingCreateApp(instance.GUID, app.GUID).WithJSONParameters(`{"mount":"/data"}`))
		wait(ctx, job, err)

		By("uploading and staging the packaged static app")
		pkg, err := cf.Packages.Create(ctx, resource.NewPackageCreate(app.GUID))
		Expect(err).NotTo(HaveOccurred())
		archive, err := appArchive(cfg.AppBinary)
		Expect(err).NotTo(HaveOccurred())
		_, err = cf.Packages.Upload(ctx, pkg.GUID, archive)
		Expect(err).NotTo(HaveOccurred())
		Expect(cf.Packages.PollReady(ctx, pkg.GUID, poll)).To(Succeed())
		build, err := cf.Builds.Create(ctx, resource.NewBuildCreate(pkg.GUID))
		Expect(err).NotTo(HaveOccurred())
		Expect(cf.Builds.PollStaged(ctx, build.GUID, poll)).To(Succeed())
		build, err = cf.Builds.Get(ctx, build.GUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(build.Droplet).NotTo(BeNil())
		_, err = cf.Droplets.SetCurrentAssociationForApp(ctx, app.GUID, build.Droplet.GUID)
		Expect(err).NotTo(HaveOccurred())

		By("mapping a route and starting exactly one app instance")
		domains := client.NewDomainListOptions()
		domains.Names.EqualTo(cfg.AppsDomain)
		domain, err := cf.Domains.Single(ctx, domains)
		Expect(err).NotTo(HaveOccurred())
		routeRequest := resource.NewRouteCreate(domain.GUID, space.GUID)
		routeRequest.Host = &name
		route, err := cf.Routes.Create(ctx, routeRequest)
		Expect(err).NotTo(HaveOccurred())
		cleanup("route", func(ctx context.Context) (string, error) { return cf.Routes.Delete(ctx, route.GUID) })
		_, err = cf.Routes.InsertDestinations(ctx, route.GUID, []*resource.RouteDestinationInsertOrReplace{resource.NewRouteDestinationInsertOrReplace(app.GUID)})
		Expect(err).NotTo(HaveOccurred())
		processes := client.NewProcessOptions()
		processes.Types.EqualTo("web")
		process, err := cf.Processes.SingleForApp(ctx, app.GUID, processes)
		Expect(err).NotTo(HaveOccurred())
		_, err = cf.Processes.Scale(ctx, process.GUID, resource.NewProcessScale().WithInstances(1).WithMemoryInMB(64).WithDiskInMB(128))
		Expect(err).NotTo(HaveOccurred())
		_, err = cf.Applications.Start(ctx, app.GUID)
		Expect(err).NotTo(HaveOccurred())

		baseURL := "https://" + route.URL
		request := func(method, path, body string, status int) (string, error) {
			req, err := http.NewRequestWithContext(ctx, method, baseURL+path, strings.NewReader(body))
			if err != nil {
				return "", err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			response, err := httpClient.Do(req)
			if err != nil {
				return "", err
			}
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			if err != nil {
				return "", err
			}
			if response.StatusCode != status {
				return "", fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, data)
			}
			return string(data), nil
		}
		var identity string
		Eventually(func() error {
			var err error
			identity, err = request("GET", "/identity", "", 200)
			return err
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		Expect(identity).NotTo(BeEmpty())

		By("posting a random GUID and reading it back")
		var uuid [16]byte
		_, err = rand.Read(uuid[:])
		Expect(err).NotTo(HaveOccurred())
		uuid[6] = (uuid[6] & 0x0f) | 0x40
		uuid[8] = (uuid[8] & 0x3f) | 0x80
		guid := fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
		_, err = request("POST", "/file", guid, 204)
		Expect(err).NotTo(HaveOccurred())
		data, err := request("GET", "/file", "", 200)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal(guid))

		By("restarting through CAPI and waiting for a different process identity")
		_, err = cf.Applications.Restart(ctx, app.GUID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) { return request("GET", "/identity", "", 200) }, 5*time.Minute, 2*time.Second).Should(And(Not(BeEmpty()), Not(Equal(identity))))
		By("retrieving the same GUID from the remounted filesystem")
		data, err = request("GET", "/file", "", 200)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal(guid))
	}, SpecTimeout(25*time.Minute))
})

func appArchive(path string) (*bytes.Reader, error) {
	binary, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: "test-app", Method: zip.Deflate}
	header.SetMode(0755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return nil, err
	}
	if _, err = entry.Write(binary); err != nil {
		return nil, err
	}
	procfile, err := writer.Create("Procfile")
	if err != nil {
		return nil, err
	}
	if _, err = io.WriteString(procfile, "web: ./test-app\n"); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}
