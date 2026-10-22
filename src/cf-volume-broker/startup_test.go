package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Broker startup configuration", func() {
	var cfg config
	var path string
	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		path = filepath.Join(dir, "config.json")
		cfg = config{}
		cfg.Broker.Root = filepath.Join(dir, "mount")
		cfg.Broker.Username = "broker"
		cfg.Broker.Password = "secret"
		cfg.Broker.DirectoryUID = os.Getuid()
		cfg.Broker.DirectoryGID = os.Getgid()
	})
	writeConfig := func() {
		data, err := json.Marshal(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
	}
	It("wires readiness, default catalog, authentication and healthy routes", func() {
		writeConfig()
		var timeout time.Duration
		server, err := configuredServer(path, func(root string, duration time.Duration) error {
			Expect(root).To(Equal(cfg.Broker.Root))
			timeout = duration
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(timeout).To(Equal(60 * time.Second))
		Expect(server.Addr).To(Equal("0.0.0.0:8080"))
		health := httptest.NewRecorder()
		server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
		Expect(health.Code).To(Equal(http.StatusNoContent))
		request := httptest.NewRequest(http.MethodGet, "/v2/catalog", nil)
		request.SetBasicAuth("broker", "secret")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		Expect(response.Code).To(Equal(http.StatusOK))
		Expect(response.Body.String()).To(ContainSubstring(planID))
	})
	It("honors explicit listen address and readiness timeout", func() {
		cfg.Broker.ListenAddr = "127.0.0.1:9090"
		cfg.Broker.MountReadyTimeout = 12
		cfg.Broker.DeletionThreads = 2
		writeConfig()
		server, err := configuredServer(path, func(_ string, timeout time.Duration) error { Expect(timeout).To(Equal(12 * time.Second)); return nil })
		Expect(err).NotTo(HaveOccurred())
		Expect(server.Addr).To(Equal("127.0.0.1:9090"))
	})
	It("rejects missing, corrupt and incomplete configuration", func() {
		_, err := configuredServer(path, nil)
		Expect(err).To(HaveOccurred())
		Expect(os.WriteFile(path, []byte("broken"), 0600)).To(Succeed())
		_, err = configuredServer(path, nil)
		Expect(err).To(HaveOccurred())
		cfg.Broker.Password = ""
		writeConfig()
		_, err = configuredServer(path, nil)
		Expect(err).To(MatchError("broker root, username and password are required"))
	})
	It("rejects invalid plans and deletion concurrency before waiting", func() {
		cfg.Broker.Plans = []plan{}
		writeConfig()
		_, err := configuredServer(path, nil)
		Expect(err).To(HaveOccurred())
		cfg.Broker.Plans = nil
		for _, threads := range []int{-1, 256} {
			cfg.Broker.DeletionThreads = threads
			writeConfig()
			_, err = configuredServer(path, nil)
			Expect(err).To(MatchError("deletionThreads must be between 1 and 255"))
		}
	})
	It("propagates inaccessible mount roots and backend readiness failures", func() {
		cfg.Broker.Root = filepath.Join(path, "nested")
		writeConfig()
		_, err := configuredServer(path, nil)
		Expect(err).To(HaveOccurred())
		cfg.Broker.Root = filepath.Join(filepath.Dir(path), "mount")
		writeConfig()
		_, err = configuredServer(path, func(string, time.Duration) error { return errors.New("backend unavailable") })
		Expect(err).To(MatchError("backend unavailable"))
	})
	It("preserves the previous instance record when atomic replacement fails", func() {
		b := &broker{root: cfg.Broker.Root}
		metadata := &instanceMetadata{Plan: defaultPlans()[0], Ready: true}
		Expect(b.writeInstanceMetadata(testInstanceID, metadata)).To(Succeed())
		path := b.metadataPath(testInstanceID)
		Expect(os.Remove(path)).To(Succeed())
		Expect(os.Mkdir(path, 0700)).To(Succeed())
		Expect(b.writeInstanceMetadata(testInstanceID, metadata)).To(HaveOccurred())
		Expect(path).To(BeADirectory())
		files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".instance-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(BeEmpty())
	})
})
