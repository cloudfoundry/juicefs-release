package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/cf-volume-driver/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDriver(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Volume Driver Suite") }

const testVolumeID = "6a8cc1ed-a799-43c8-8ea2-38c4cf14fb19"
const testSubdir = "/cf-volumes/" + testVolumeID

var _ = Describe("Volume lifecycle", func() {
	var d *driver
	var mounts *fakes.FakeMounts
	var factory *fakes.FakeFactory
	var child *fakes.FakeProcess
	var exited chan error
	call := func(handler http.HandlerFunc, body any) map[string]any {
		encoded, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded)))
		var response map[string]any
		Expect(json.Unmarshal(w.Body.Bytes(), &response)).To(Succeed())
		return response
	}
	BeforeEach(func() {
		root := GinkgoT().TempDir()
		mounts, factory, child = &fakes.FakeMounts{}, &fakes.FakeFactory{}, &fakes.FakeProcess{}
		exited = make(chan error, 1)
		exitChannel := exited
		child.WaitStub = func() error { return <-exitChannel }
		child.SignalStub = func(os.Signal) error { exitChannel <- nil; return nil }
		factory.NewReturns(child)
		d = &driver{volumes: map[string]*volume{}, statePath: filepath.Join(root, "state.json"), mountRoot: filepath.Join(root, "mounts"), cacheRoot: filepath.Join(root, "cache"), baseConfig: map[string]any{"metadata": map[string]any{"host": "metadata"}, "volume": map[string]any{"name": "volume"}}, mounts: mounts, processes: factory, stop: make(chan struct{})}
	})
	create := func() {
		Expect(call(d.handleCreate, createRequest{Name: testVolumeID, Opts: map[string]any{"subdir": testSubdir}})["Err"]).To(BeEmpty())
	}
	It("rejects malformed identifiers and subdirectories outside the instance", func() {
		for _, subdir := range []string{"/", "/cf-volumes/other", "/cf-volumes/../secret", testSubdir + "/nested"} {
			Expect(call(d.handleCreate, createRequest{Name: testVolumeID, Opts: map[string]any{"subdir": subdir}})["Err"]).NotTo(BeEmpty())
		}
		Expect(call(d.handleCreate, createRequest{Name: "invalid"})["Err"]).NotTo(BeEmpty())
	})
	It("persists only validated instance subdirectories and rejects corrupt state", func() {
		create()
		d.volumes = map[string]*volume{}
		Expect(d.load()).To(Succeed())
		Expect(d.volumes[testVolumeID].Subdir).To(Equal(testSubdir))
		Expect(os.WriteFile(d.statePath, []byte(`{"invalid":"/"}`), 0600)).To(Succeed())
		d.volumes = map[string]*volume{}
		Expect(d.load()).To(Succeed())
		Expect(d.volumes).To(BeEmpty())
		Expect(os.WriteFile(d.statePath, []byte(`broken`), 0600)).To(Succeed())
		Expect(d.load()).NotTo(Succeed())
	})
	It("mounts once, shares references, and terminates only the final reference", func() {
		create()
		mounts.MountedReturns(true, nil)
		response := call(d.handleMount, nameRequest{Name: testVolumeID})
		Expect(response["Err"]).To(BeEmpty())
		Expect(response["Mountpoint"]).To(Equal(filepath.Join(d.mountRoot, testVolumeID)))
		path, args := factory.NewArgsForCall(0)
		Expect(path).To(Equal(launcherBin))
		Expect(args[0]).To(Equal("mount"))
		contents, err := os.ReadFile(args[1])
		Expect(err).NotTo(HaveOccurred())
		Expect(string(contents)).To(ContainSubstring(testSubdir))
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(factory.NewCallCount()).To(Equal(1))
		Expect(call(d.handleRemove, nameRequest{Name: testVolumeID})["Err"]).NotTo(BeEmpty())
		Expect(call(d.handleUnmount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(child.SignalCallCount()).To(BeZero())
		Expect(call(d.handleUnmount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(child.SignalArgsForCall(0)).To(Equal(syscall.SIGTERM))
		Expect(d.volumes[testVolumeID].MountCount).To(BeZero())
		Expect(call(d.handleRemove, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(d.volumes).To(BeEmpty())
	})
	It("reports start failures without persisting a live mount", func() {
		create()
		child.StartReturns(errors.New("exec failed"))
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(ContainSubstring("exec failed"))
		Expect(d.volumes[testVolumeID].MountCount).To(BeZero())
	})
	It("terminates a process when mount inspection fails", func() {
		create()
		mounts.MountedReturns(false, errors.New("mountinfo unavailable"))
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(ContainSubstring("mountinfo unavailable"))
		Expect(child.SignalCallCount()).To(Equal(1))
		Eventually(func() bool { return len(exited) == 0 }).Should(BeTrue())
	})
	It("rejects missing and orphaned mount references", func() {
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(ContainSubstring("created"))
		create()
		d.volumes[testVolumeID].MountCount = 1
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(ContainSubstring("no mount process"))
	})
	It("reports previous mount exits instead of handing out a dead mount", func() {
		create()
		d.volumes[testVolumeID].MountCount = 1
		d.volumes[testVolumeID].cmd = child
		d.volumes[testVolumeID].done = make(chan error, 1)
		d.volumes[testVolumeID].done <- errors.New("crashed")
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(ContainSubstring("crashed"))
	})
	It("recovers stale mounts without inventing references", func() {
		create()
		mounts.MountedReturns(true, nil)
		Expect(d.recoverMounts()).To(Succeed())
		Expect(mounts.UnmountArgsForCall(0)).To(Equal(filepath.Join(d.mountRoot, testVolumeID)))
		Expect(d.volumes[testVolumeID].MountCount).To(BeZero())
		mounts.UnmountReturns(errors.New("busy"))
		Expect(d.recoverMounts()).To(MatchError(ContainSubstring("busy")))
		mounts.MountedReturns(false, errors.New("inspection failed"))
		Expect(d.recoverMounts()).To(MatchError("inspection failed"))
	})
	It("handles process exits, timeout and shutdown while waiting for mount readiness", func() {
		done := make(chan error, 1)
		done <- errors.New("failed")
		Expect(d.waitMounted("/mnt", done, time.Hour)).To(MatchError(ContainSubstring("failed")))
		done <- nil
		Expect(d.waitMounted("/mnt", done, time.Hour)).To(MatchError(ContainSubstring("exited before")))
		Expect(d.waitMounted("/mnt", done, 0)).To(MatchError(ContainSubstring("timed out")))
		close(d.stop)
		Expect(d.waitMounted("/mnt", done, time.Hour)).To(MatchError("driver shutting down"))
	})
	It("shuts down every active child", func() {
		create()
		mounts.MountedReturns(true, nil)
		call(d.handleMount, nameRequest{Name: testVolumeID})
		d.unmountAll()
		Expect(child.SignalCallCount()).To(Equal(1))
	})
	It("kills and reaps a child that ignores graceful shutdown", func() {
		create()
		mounts.MountedReturns(true, nil)
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		d.shutdownTimeout = time.Nanosecond
		child.SignalStub = nil
		exitChannel := exited
		child.KillStub = func() error { exitChannel <- errors.New("killed"); return nil }
		Expect(call(d.handleUnmount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(child.KillCallCount()).To(Equal(1))
		Expect(d.volumes[testVolumeID].MountCount).To(BeZero())
	})
	It("returns paths and lists and makes absent unmount/remove idempotent", func() {
		create()
		Expect(call(d.handleGet, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(call(d.handlePath, nameRequest{Name: testVolumeID})["Err"]).To(Equal("volume is not mounted"))
		Expect(call(d.handleList, nameRequest{})["Volumes"]).To(HaveLen(1))
		Expect(call(d.handleUnmount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(call(d.handleRemove, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(call(d.handleUnmount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(call(d.handleRemove, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
	})
	It("creates private mount overrides and cleans them up", func() {
		path := filepath.Join(GinkgoT().TempDir(), "base.json")
		Expect(os.WriteFile(path, []byte(`{"volume":{"subdir":"`+testSubdir+`"}}`), 0600)).To(Succeed())
		override, cleanup, err := writeMountOverride(path, "/mount", "/cache")
		Expect(err).NotTo(HaveOccurred())
		contents, err := os.ReadFile(override)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(contents)).To(ContainSubstring(testSubdir))
		cleanup()
		Expect(override).NotTo(BeAnExistingFile())
		Expect(os.WriteFile(path, []byte(`{}`), 0600)).To(Succeed())
		_, _, err = writeMountOverride(path, "/mount", "/cache")
		Expect(err).To(HaveOccurred())
	})
	It("publishes driver discovery files and reports filesystem errors", func() {
		dir := GinkgoT().TempDir()
		Expect(registerDriver(dir)).To(Succeed())
		data, err := os.ReadFile(filepath.Join(dir, driverName+".json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("http://127.0.0.1:7591"))
		Expect(registerDriver(filepath.Join(dir, driverName+".spec", "nested"))).NotTo(Succeed())
	})
	It("rejects malformed requests to every volume endpoint", func() {
		for _, handler := range []http.HandlerFunc{d.handleCreate, d.handleMount, d.handleUnmount, d.handleRemove, d.handleGet, d.handlePath} {
			Expect(call(handler, nameRequest{Name: "invalid"})["Err"]).NotTo(BeEmpty())
		}
		Expect(call(d.handleGet, nameRequest{Name: testVolumeID})["Err"]).NotTo(BeEmpty())
		Expect(call(d.handlePath, nameRequest{Name: testVolumeID})["Err"]).NotTo(BeEmpty())
		Expect(validID("6a8cc1ed_a799-43c8-8ea2-38c4cf14fb19")).To(BeFalse())
		Expect(validID("za8cc1ed-a799-43c8-8ea2-38c4cf14fb19")).To(BeFalse())
	})
	It("does not report persistence failures as successful create or remove", func() {
		create()
		original := d.statePath
		d.statePath = filepath.Join(d.statePath, "invalid")
		Expect(call(d.handleCreate, createRequest{Name: testVolumeID, Opts: map[string]any{"subdir": testSubdir}})["Err"]).NotTo(BeEmpty())
		Expect(call(d.handleRemove, nameRequest{Name: testVolumeID})["Err"]).NotTo(BeEmpty())
		Expect(d.volumes).To(HaveKey(testVolumeID))
		d.statePath = original
		Expect(d.load()).To(Succeed())
	})
	It("returns the mounted path and gracefully stops a real subprocess", func() {
		create()
		mounts.MountedReturns(true, nil)
		Expect(call(d.handleMount, nameRequest{Name: testVolumeID})["Err"]).To(BeEmpty())
		Expect(call(d.handlePath, nameRequest{Name: testVolumeID})["Mountpoint"]).To(Equal(filepath.Join(d.mountRoot, testVolumeID)))
		d.unmountAll()
		Expect(runCommandUntilSignal(exec.Command("/bin/sh", "-c", "exit 0"))).To(Succeed())
		Expect(runCommandUntilSignal(exec.Command("/nonexistent"))).To(HaveOccurred())
	})
	It("inspects the host mount table without requiring a FUSE mount", func() {
		mounted, err := isMounted("/")
		Expect(err).NotTo(HaveOccurred())
		Expect(mounted).To(BeTrue())
		_, err = (hostMounts{}).Mounted("/nonexistent")
		Expect(err).NotTo(HaveOccurred())
		Expect(mountWithOverrides("/nonexistent", "/mount", "/cache")).To(HaveOccurred())
	})
})
