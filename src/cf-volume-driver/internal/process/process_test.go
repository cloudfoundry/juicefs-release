package process_test

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/cf-volume-driver/internal/process"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestProcess(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Process Adapter Suite") }

var _ = Describe("Process factory", func() {
	It("starts and reaps a successful child", func() {
		child := (process.FactoryImpl{}).New("/bin/sh", "-c", "exit 0")
		Expect(child.Start()).To(Succeed())
		Expect(child.Wait()).To(Succeed())
	})
	It("reports start errors", func() { child := (process.FactoryImpl{}).New("/nonexistent"); Expect(child.Start()).NotTo(Succeed()) })
	It("signals and reaps a child", func() {
		path, err := exec.LookPath("sleep")
		Expect(err).NotTo(HaveOccurred())
		child := (process.FactoryImpl{}).New(path, "30")
		Expect(child.Start()).To(Succeed())
		Expect(child.Signal(syscall.SIGTERM)).To(Succeed())
		Expect(child.Wait()).To(HaveOccurred())
	})
	It("kills and reaps a child", func() {
		path, err := exec.LookPath("sleep")
		Expect(err).NotTo(HaveOccurred())
		child := (process.FactoryImpl{}).New(path, "30")
		Expect(child.Start()).To(Succeed())
		Expect(child.Kill()).To(Succeed())
		Expect(child.Wait()).To(HaveOccurred())
	})
})
