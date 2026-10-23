package testapp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/juicefs-acceptance-tests/internal/testapp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestApp(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Acceptance app")
}

var _ = Describe("the persistence app", func() {
	It("retains the exact submitted bytes across handler replacement, but changes identity", func() {
		dir := GinkgoT().TempDir()
		first, err := testapp.New(dir, "secret")
		Expect(err).NotTo(HaveOccurred())
		request := func(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		Expect(request(first, "GET", "/file", "", "secret").Code).To(Equal(404))
		Expect(request(first, "POST", "/file", "value", "wrong").Code).To(Equal(401))
		Expect(request(first, "POST", "/file", strings.Repeat("x", 129), "secret").Code).To(Equal(400))
		guid := "853a4d78-21c3-4d47-882c-57b28510feba"
		Expect(request(first, "POST", "/file", guid, "secret").Code).To(Equal(204))
		second, err := testapp.New(dir, "secret")
		Expect(err).NotTo(HaveOccurred())
		body, err := io.ReadAll(request(second, "GET", "/file", "", "secret").Result().Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal(guid))
		Expect(request(second, "GET", "/identity", "", "secret").Body.String()).NotTo(Equal(request(first, "GET", "/identity", "", "secret").Body.String()))
	})
	It("fails startup when the mount directory is absent", func() {
		_, err := testapp.New(GinkgoT().TempDir()+"/absent", "secret")
		Expect(err).To(HaveOccurred())
	})
})
