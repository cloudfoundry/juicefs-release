package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudfoundry/juicefs-release/src/broker-registration/internal/cli"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCLI(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "CF CLI Adapter Suite") }

var _ = Describe("CF CLI runner", func() {
	It("reports missing CLI and child errors while capturing output", func() {
		dir := GinkgoT().TempDir()
		GinkgoT().Setenv("PATH", dir)
		runner := cli.ExecRunner{}
		_, err := runner.Output(nil, "auth")
		Expect(err).To(MatchError(ContainSubstring("locate vendored")))
		path := filepath.Join(dir, "cf")
		Expect(os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' \"$VALUE\"\n"), 0700)).To(Succeed())
		out, err := runner.Output([]string{"VALUE=passed"}, "auth")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("passed"))
		Expect(os.WriteFile(path, []byte("#!/bin/sh\nprintf failed >&2\nexit 7\n"), 0700)).To(Succeed())
		_, err = runner.Output(nil, "auth")
		Expect(err).To(MatchError(ContainSubstring("failed")))
	})
})
