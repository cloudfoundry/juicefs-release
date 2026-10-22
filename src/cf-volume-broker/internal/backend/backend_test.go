package backend_test

import (
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/cf-volume-broker/internal/backend"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestBackend(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Broker Command Adapter Suite")
}

var _ = Describe("Exec runner", func() {
	It("reports command failures and executes arguments without shell interpretation", func() {
		runner := backend.ExecRunner{}
		Expect(runner.Run("/bin/sh", "-c", "exit 0")).To(Succeed())
		Expect(runner.Run("/bin/sh", "-c", "exit 7")).To(MatchError(ContainSubstring("exit status")))
		Expect(runner.Run("/nonexistent")).To(HaveOccurred())
	})
})
