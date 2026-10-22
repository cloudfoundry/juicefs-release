package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/cf-volume-broker/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestBroker(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Volume Broker Suite") }

const testInstanceID = "6a8cc1ed-a799-43c8-8ea2-38c4cf14fb19"

var _ = Describe("Service instance lifecycle", func() {
	var b *broker
	call := func(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(body))
		r.SetPathValue("id", testInstanceID)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	provision := func() *httptest.ResponseRecorder {
		return call(b.provision, `{"service_id":"`+serviceID+`","plan_id":"`+planID+`"}`)
	}
	BeforeEach(func() { b = &broker{root: GinkgoT().TempDir(), username: "broker", password: "secret"} })
	It("provisions, binds, retrieves, unbinds and deprovisions an isolated directory", func() {
		Expect(provision().Code).To(Equal(http.StatusCreated))
		Expect(provision().Code).To(Equal(http.StatusOK))
		path := filepath.Join(b.root, "cf-volumes", testInstanceID)
		Expect(path).To(BeADirectory())
		w := call(b.bind, `{"app_guid":"app","parameters":{"mount":"/data"}}`)
		Expect(w.Code).To(Equal(http.StatusCreated))
		Expect(w.Body.String()).To(ContainSubstring(`"subdir":"/cf-volumes/` + testInstanceID + `"`))
		Expect(w.Body.String()).To(ContainSubstring(`"container_dir":"/data"`))
		Expect(call(b.getInstance, "").Code).To(Equal(http.StatusOK))
		Expect(call(b.unbind, "").Code).To(Equal(http.StatusOK))
		Expect(call(b.deprovision, "").Code).To(Equal(http.StatusOK))
		Expect(path).NotTo(BeAnExistingFile())
		Expect(b.metadataPath(testInstanceID)).NotTo(BeAnExistingFile())
		Expect(call(b.deprovision, "").Code).To(Equal(http.StatusOK))
	})
	It("retains pending plan limits across failed quota application and retries", func() {
		p := plan{ID: planID, Name: "small", Description: "small", CapacityGiB: 1, Inodes: 10, Free: true}
		b.plans = []plan{p}
		calls := 0
		b.applyQuota = func(path string, selected plan) error {
			calls++
			Expect(path).To(Equal("/cf-volumes/" + testInstanceID))
			Expect(selected).To(Equal(p))
			if calls == 1 {
				return errors.New("quota unavailable")
			}
			return nil
		}
		Expect(provision().Code).To(Equal(http.StatusInternalServerError))
		metadata, err := b.readInstanceMetadata(testInstanceID)
		Expect(err).NotTo(HaveOccurred())
		Expect(metadata.Ready).To(BeFalse())
		b.plans[0].CapacityGiB = 5
		Expect(provision().Code).To(Equal(http.StatusCreated))
		Expect(provision().Code).To(Equal(http.StatusOK))
		Expect(calls).To(Equal(2))
		Expect(call(b.provision, `{"service_id":"`+serviceID+`","plan_id":"different"}`).Code).To(Equal(http.StatusConflict))
	})
	It("keeps metadata when backend deletion fails", func() {
		Expect(provision().Code).To(Equal(http.StatusCreated))
		b.removeDirectory = func(string) error { return errors.New("backend unavailable") }
		Expect(call(b.deprovision, "").Code).To(Equal(http.StatusInternalServerError))
		Expect(b.metadataPath(testInstanceID)).To(BeAnExistingFile())
		b.removeDirectory = os.RemoveAll
		Expect(call(b.deprovision, "").Code).To(Equal(http.StatusOK))
	})
	It("rejects malformed IDs, requests, service and plan identifiers", func() {
		for _, id := range []string{"../outside", "not-a-guid", "00000000-0000-0000-0000-00000000000/"} {
			_, err := b.instancePath(id)
			Expect(err).To(HaveOccurred())
		}
		for _, body := range []string{`broken`, `{}`, `{"service_id":"wrong","plan_id":"` + planID + `"}`, `{"service_id":"` + serviceID + `","plan_id":"unknown"}`} {
			Expect(call(b.provision, body).Code).NotTo(Equal(http.StatusCreated))
		}
		Expect(call(b.bind, `{"app_guid":"app"}`).Code).To(Equal(http.StatusNotFound))
	})
	DescribeTable("validates binding mount paths", func(parameters, path string, status int) {
		Expect(provision().Code).To(Equal(http.StatusCreated))
		w := call(b.bind, `{"app_guid":"app","parameters":`+parameters+`}`)
		Expect(w.Code).To(Equal(status))
		if path != "" {
			Expect(w.Body.String()).To(ContainSubstring(`"container_dir":"` + path + `"`))
		}
	}, Entry("default", `{}`, "/var/vcap/data/"+testInstanceID, http.StatusCreated), Entry("custom", `{"mount":"/data"}`, "/data", http.StatusCreated), Entry("relative", `{"mount":"data"}`, "", http.StatusBadRequest), Entry("root", `{"mount":"/"}`, "", http.StatusBadRequest), Entry("traversal", `{"mount":"/data/../etc"}`, "", http.StatusBadRequest), Entry("empty", `{"mount":""}`, "", http.StatusBadRequest), Entry("null", `{"mount":null}`, "", http.StatusBadRequest), Entry("numeric", `{"mount":42}`, "", http.StatusBadRequest), Entry("control", `{"mount":"/data\n"}`, "", http.StatusBadRequest))
	It("rejects binding to pending instances or without an app", func() {
		Expect(provision().Code).To(Equal(http.StatusCreated))
		Expect(call(b.bind, `{}`).Code).To(Equal(http.StatusBadRequest))
		Expect(b.writeInstanceMetadata(testInstanceID, &instanceMetadata{Plan: defaultPlans()[0]})).To(Succeed())
		Expect(call(b.bind, `{"app_guid":"app"}`).Code).To(Equal(http.StatusConflict))
	})
	It("exposes the catalog only to authorized callers", func() {
		for _, credentials := range [][2]string{{"", ""}, {"broker", "wrong"}, {"broker", "secret"}} {
			r := httptest.NewRequest(http.MethodGet, "/v2/catalog", nil)
			r.SetBasicAuth(credentials[0], credentials[1])
			w := httptest.NewRecorder()
			b.auth(b.catalog)(w, r)
			if credentials[1] == "secret" {
				Expect(w.Code).To(Equal(http.StatusOK))
				Expect(w.Body.String()).To(ContainSubstring(serviceID))
			} else {
				Expect(w.Code).To(Equal(http.StatusUnauthorized))
			}
		}
	})
	It("rejects invalid or duplicate plans", func() {
		p := defaultPlans()[0]
		for _, plans := range [][]plan{{}, {p, p}, {{ID: "id", Name: "bad name", Description: "bad"}}, {{ID: "id", Name: "small", Description: "bad", CapacityGiB: -1}}} {
			Expect(validatePlans(plans)).NotTo(Succeed())
		}
		Expect(validatePlans(defaultPlans())).To(Succeed())
	})
	It("keeps existing directory permissions and validates instance metadata", func() {
		path := filepath.Join(b.root, "cf-volumes", testInstanceID)
		Expect(createInstanceDirectory(path, os.Getuid(), os.Getgid())).To(Succeed())
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0770)))
		Expect(os.Chmod(path, 0700)).To(Succeed())
		Expect(createInstanceDirectory(path, os.Getuid(), os.Getgid())).To(Succeed())
		info, err = os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0700)))
		Expect(b.writeInstanceMetadata(testInstanceID, &instanceMetadata{Plan: defaultPlans()[0], Ready: true})).To(Succeed())
		Expect(os.WriteFile(b.metadataPath(testInstanceID), []byte(`broken`), 0600)).To(Succeed())
		_, err = b.readInstanceMetadata(testInstanceID)
		Expect(err).To(HaveOccurred())
	})
	It("bounds recursive deletion workers and verifies deletion really completed", func() {
		path := filepath.Join(b.root, "instance")
		Expect(os.Mkdir(path, 0700)).To(Succeed())
		runner := &fakes.FakeRunner{}
		execute := func(args ...string) error { return runner.Run("juicefs", args...) }
		Expect(removeJuiceFSDirectory(path, 4, execute)).NotTo(Succeed())
		_, args := runner.RunArgsForCall(0)
		Expect(args).To(Equal([]string{"rmr", "--skip-trash", "--threads", "4", path}))
		runner.RunStub = func(string, ...string) error { return os.RemoveAll(path) }
		Expect(removeJuiceFSDirectory(path, 4, execute)).To(Succeed())
		Expect(removeJuiceFSDirectory(path, 4, execute)).To(MatchError(os.ErrNotExist))
		Expect(runner.RunCallCount()).To(Equal(2))
	})
	It("passes quota limits to the launcher and skips unlimited plans", func() {
		runner := &fakes.FakeRunner{}
		Expect(applyDirectoryQuotaWith(runner, "/cf-volumes/id", plan{})).To(Succeed())
		Expect(runner.RunCallCount()).To(BeZero())
		Expect(applyDirectoryQuotaWith(runner, "/cf-volumes/id", plan{CapacityGiB: 2, Inodes: 100})).To(Succeed())
		path, args := runner.RunArgsForCall(0)
		Expect(path).To(Equal("/var/vcap/packages/juicefs-launcher/bin/juicefs-launcher"))
		Expect(args).To(Equal([]string{"quota", "/var/vcap/jobs/juicefs-volume/config/config.json", "/cf-volumes/id", "2", "100"}))
		runner.RunReturns(errors.New("quota failed"))
		Expect(applyDirectoryQuotaWith(runner, "/cf-volumes/id", plan{Inodes: 1})).To(MatchError("quota failed"))
	})
	It("never removes parents outside the service root or nonempty parents", func() {
		stop := filepath.Join(b.root, "cf-volumes")
		nested := filepath.Join(stop, "a", "b")
		Expect(os.MkdirAll(nested, 0700)).To(Succeed())
		Expect(removeEmptyParents(nested, stop)).To(Succeed())
		Expect(stop).To(BeADirectory())
		Expect(removeEmptyParents(b.root, stop)).NotTo(Succeed())
		Expect(os.MkdirAll(nested, 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(nested, "state"), nil, 0600)).To(Succeed())
		Expect(removeEmptyParents(nested, stop)).To(Succeed())
		Expect(nested).To(BeADirectory())
	})
	It("encodes persisted plan metadata", func() {
		Expect(provision().Code).To(Equal(http.StatusCreated))
		data, err := os.ReadFile(b.metadataPath(testInstanceID))
		Expect(err).NotTo(HaveOccurred())
		var metadata instanceMetadata
		Expect(json.Unmarshal(data, &metadata)).To(Succeed())
		Expect(metadata.Ready).To(BeTrue())
	})
	It("reports a missing or incomplete instance and malformed paths", func() {
		Expect(call(b.getInstance, "").Code).NotTo(Equal(http.StatusOK))
		Expect(serviceInstanceExists(filepath.Join(b.root, "cf-volumes"), testInstanceID)).To(BeFalse())
		Expect(provision().Code).To(Equal(http.StatusCreated))
		Expect(serviceInstanceExists(filepath.Join(b.root, "cf-volumes"), testInstanceID)).To(BeTrue())
		Expect(os.RemoveAll(filepath.Join(b.root, "cf-volumes", testInstanceID))).To(Succeed())
		Expect(provision().Code).To(Equal(http.StatusInternalServerError))
		for _, handler := range []http.HandlerFunc{b.provision, b.bind, b.getInstance, b.deprovision} {
			r := httptest.NewRequest(http.MethodPut, "/", nil)
			r.SetPathValue("id", "invalid")
			w := httptest.NewRecorder()
			handler(w, r)
			Expect(w.Code).NotTo(Equal(http.StatusOK))
		}
	})
	It("rejects directory creation failures, absent quota backend and corrupted records", func() {
		b.createDirectory = func(string) error { return errors.New("create failed") }
		Expect(provision().Code).To(Equal(http.StatusInternalServerError))
		b.createDirectory = nil
		Expect(os.Remove(b.metadataPath(testInstanceID))).To(Succeed())
		b.plans = []plan{{ID: planID, Name: "small", Description: "small", Inodes: 10}}
		Expect(provision().Code).To(Equal(http.StatusInternalServerError))
		Expect(os.WriteFile(b.metadataPath(testInstanceID), []byte(`{"plan":{}}`), 0600)).To(Succeed())
		Expect(provision().Code).To(Equal(http.StatusInternalServerError))
		Expect(call(b.bind, `{"app_guid":"app"}`).Code).To(Equal(http.StatusInternalServerError))
	})
	It("prevents symlink escapes from the instance root", func() {
		outside := GinkgoT().TempDir()
		Expect(os.Symlink(outside, filepath.Join(b.root, "cf-volumes"))).To(Succeed())
		_, err := b.instancePath(testInstanceID)
		Expect(err).To(HaveOccurred())
	})
	It("checks mount readiness and decodes escaped mount fields", func() {
		Expect(unescapeMountField(`/path\040with\011tab\134slash`)).To(Equal("/path with\ttab\\slash"))
		_, err := isMounted(b.root)
		Expect(err).NotTo(HaveOccurred())
		Expect(waitForMount(b.root, 0*time.Second)).To(HaveOccurred())
	})
})
