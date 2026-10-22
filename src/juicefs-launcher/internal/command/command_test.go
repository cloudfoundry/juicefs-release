package command_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/juicefs-launcher/internal/command"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCommand(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Command Adapter Suite") }

var _ = Describe("Exec runner", func() {
	It("returns start and nonzero exit errors", func() {
		runner := command.ExecRunner{}
		Expect(runner.Run("/nonexistent", nil, nil, nil)).To(HaveOccurred())
		Expect(runner.Run("/bin/sh", []string{"-c", "exit 7"}, os.Environ(), nil)).To(HaveOccurred())
		Expect(runner.Run("/bin/sh", []string{"-c", "test \"$VALUE\" = expected"}, []string{"VALUE=expected"}, nil)).To(Succeed())
	})
	It("forwards TERM to the child and waits for its handler", func() {
		ready := filepath.Join(GinkgoT().TempDir(), "ready")
		signals := make(chan os.Signal, 1)
		done := make(chan error, 1)
		go func() {
			done <- (command.ExecRunner{}).Run("/bin/sh", []string{"-c", `trap 'exit 0' TERM; touch "$1"; while :; do sleep 0.05; done`, "sh", ready}, os.Environ(), signals)
		}()
		Eventually(func() bool { _, err := os.Stat(ready); return err == nil }, time.Second*5).Should(BeTrue())
		signals <- syscall.SIGTERM
		Eventually(done, time.Second*5).Should(Receive(BeNil()))
	})
})
