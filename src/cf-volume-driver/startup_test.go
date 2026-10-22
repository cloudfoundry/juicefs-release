package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Driver configuration and discovery", func() {
	It("loads paths without losing authoritative backend settings", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.json")
		Expect(os.WriteFile(path, []byte(`{"metadata":{"host":"database"},"volume":{"mountPoint":"/mount","cacheDir":"/cache"}}`), 0600)).To(Succeed())
		d, err := configuredDriver(path, "/state.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(d.mountRoot).To(Equal("/mount"))
		Expect(d.cacheRoot).To(Equal("/cache"))
		Expect(d.baseConfig).To(HaveKey("metadata"))
		Expect(d.stop).NotTo(BeNil())
		for _, endpoint := range []string{"/Plugin.Activate", "/VolumeDriver.Capabilities", "/VolumeDriver.List"} {
			w := httptest.NewRecorder()
			d.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, endpoint, nil))
			Expect(w.Code).To(Equal(http.StatusOK))
			Expect(w.Header().Get("Content-Type")).To(Equal("application/json"))
		}
	})
	It("rejects missing, malformed and incomplete configuration", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.json")
		_, err := configuredDriver(path, "/state")
		Expect(err).To(HaveOccurred())
		for _, data := range []string{"broken", `{}`, `{"volume":{}}`, `{"volume":{"mountPoint":"/mount"}}`, `{"volume":{"mountPoint":42,"cacheDir":"/cache"}}`} {
			Expect(os.WriteFile(path, []byte(data), 0600)).To(Succeed())
			_, err = configuredDriver(path, "/state")
			Expect(err).To(HaveOccurred())
		}
	})
	It("does not overwrite state when its temporary file or destination is blocked", func() {
		dir := GinkgoT().TempDir()
		d := &driver{volumes: map[string]*volume{}, statePath: filepath.Join(dir, "state.json")}
		Expect(os.Mkdir(d.statePath+".tmp", 0700)).To(Succeed())
		Expect(d.save()).To(HaveOccurred())
		Expect(os.Remove(d.statePath + ".tmp")).To(Succeed())
		Expect(os.Mkdir(d.statePath, 0700)).To(Succeed())
		Expect(d.save()).To(HaveOccurred())
		Expect(d.statePath).To(BeADirectory())
		Expect(d.load()).To(HaveOccurred())
	})
})
