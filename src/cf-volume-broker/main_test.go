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
)

const testInstanceID = "6a8cc1ed-a799-43c8-8ea2-38c4cf14fb19"

func TestRecursiveDeletionBoundsWorkersAndVerifiesCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), testInstanceID)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	execute := func(args ...string) error {
		calls++
		want := []string{"rmr", "--skip-trash", "--threads", "4", path}
		if len(args) != len(want) {
			t.Fatalf("unexpected arguments: %v", args)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("unexpected arguments: %v", args)
			}
		}
		return nil
	}
	if err := removeJuiceFSDirectory(path, 4, execute); err == nil {
		t.Fatal("successful CLI exit accepted while directory still exists")
	}
	if err := removeJuiceFSDirectory(path, 4, func(args ...string) error {
		if err := execute(args...); err != nil {
			return err
		}
		return os.RemoveAll(path)
	}); err != nil {
		t.Fatal(err)
	}
	if err := removeJuiceFSDirectory(path, 4, execute); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent-directory retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("absent directory invoked CLI: %d", calls)
	}
}

func TestQuotaProvisionRetriesRetainOriginalPlan(t *testing.T) {
	p := plan{ID: "small-id", Name: "small", Description: "Small", CapacityGiB: 1, Inodes: 10, Free: true}
	b := &broker{root: t.TempDir(), plans: []plan{p}}
	calls := 0
	b.applyQuota = func(path string, selected plan) error {
		calls++
		if path != "/cf-volumes/"+testInstanceID || selected != p {
			t.Fatalf("unexpected quota %s %+v", path, selected)
		}
		if calls == 1 {
			return errors.New("backend unavailable")
		}
		return nil
	}
	provision := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(`{"service_id":"`+serviceID+`","plan_id":"`+id+`"}`))
		r.SetPathValue("id", testInstanceID)
		w := httptest.NewRecorder()
		b.provision(w, r)
		return w
	}
	if w := provision(p.ID); w.Code != http.StatusInternalServerError {
		t.Fatalf("first status %d", w.Code)
	}
	metadata, err := b.readInstanceMetadata(testInstanceID)
	if err != nil || metadata == nil || metadata.Ready {
		t.Fatalf("pending metadata %+v %v", metadata, err)
	}
	b.plans[0].CapacityGiB = 5
	if w := provision(p.ID); w.Code != http.StatusCreated {
		t.Fatalf("retry status %d %s", w.Code, w.Body.String())
	}
	if w := provision(p.ID); w.Code != http.StatusOK || calls != 2 {
		t.Fatalf("idempotent status %d calls %d", w.Code, calls)
	}
	if w := provision(planID); w.Code != http.StatusConflict {
		t.Fatalf("conflict status %d", w.Code)
	}
}

func TestPlanValidation(t *testing.T) {
	p := defaultPlans()[0]
	for _, plans := range [][]plan{{}, {p, p}, {{ID: "id", Name: "bad name", Description: "bad"}}, {{ID: "id", Name: "small", Description: "bad", CapacityGiB: -1}}} {
		if err := validatePlans(plans); err == nil {
			t.Fatalf("invalid plans accepted: %+v", plans)
		}
	}
	if err := validatePlans(defaultPlans()); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionBindAndDeprovisionUseInstanceSubdirectory(t *testing.T) {
	root := t.TempDir()
	removed := ""
	b := &broker{root: root, username: "broker", password: "secret", publicURL: "http://broker", removeDirectory: func(path string) error {
		removed = path
		return os.RemoveAll(path)
	}}
	provision := httptest.NewRequest(http.MethodPut, "/v2/service_instances/"+testInstanceID, bytes.NewBufferString(`{"service_id":"`+serviceID+`","plan_id":"`+planID+`"}`))
	provision.SetPathValue("id", testInstanceID)
	w := httptest.NewRecorder()
	b.provision(w, provision)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision status = %d, body = %s", w.Code, w.Body.String())
	}
	wantPath := filepath.Join(root, "cf-volumes", testInstanceID)
	if info, err := os.Stat(wantPath); err != nil || !info.IsDir() {
		t.Fatalf("provisioned directory missing: %v", err)
	}

	bind := httptest.NewRequest(http.MethodPut, "/v2/service_instances/"+testInstanceID+"/service_bindings/bind", bytes.NewBufferString(`{"app_guid":"app-guid"}`))
	bind.SetPathValue("id", testInstanceID)
	w = httptest.NewRecorder()
	b.bind(w, bind)
	if w.Code != http.StatusCreated {
		t.Fatalf("bind status = %d, body = %s", w.Code, w.Body.String())
	}
	var response struct {
		VolumeMounts []struct {
			Device struct {
				VolumeID    string            `json:"volume_id"`
				MountConfig map[string]string `json:"mount_config"`
			} `json:"device"`
		} `json:"volume_mounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.VolumeMounts) != 1 || response.VolumeMounts[0].Device.VolumeID != testInstanceID || response.VolumeMounts[0].Device.MountConfig["subdir"] != "/cf-volumes/"+testInstanceID {
		t.Fatalf("unexpected volume mount response: %+v", response)
	}

	deprovision := httptest.NewRequest(http.MethodDelete, "/v2/service_instances/"+testInstanceID, nil)
	deprovision.SetPathValue("id", testInstanceID)
	w = httptest.NewRecorder()
	b.deprovision(w, deprovision)
	if w.Code != http.StatusOK || removed != wantPath {
		t.Fatalf("deprovision status=%d removed=%q want=%q", w.Code, removed, wantPath)
	}
}

func TestInstancePathRejectsTraversalAndMalformedIDs(t *testing.T) {
	b := &broker{root: t.TempDir()}
	for _, id := range []string{"../outside", "not-a-guid", "00000000-0000-0000-0000-00000000000/"} {
		if _, err := b.instancePath(id); err == nil {
			t.Errorf("instancePath(%q) unexpectedly succeeded", id)
		}
	}
}

func TestBindingMountParameter(t *testing.T) {
	b := &broker{root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(b.root, "cf-volumes", testInstanceID), 0770); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, parameters, wantPath string
		wantStatus                 int
	}{
		{"default", `{}`, "/var/vcap/data/" + testInstanceID, http.StatusCreated},
		{"custom", `{"mount":"/data"}`, "/data", http.StatusCreated},
		{"relative", `{"mount":"data"}`, "", http.StatusBadRequest},
		{"root", `{"mount":"/"}`, "", http.StatusBadRequest},
		{"traversal", `{"mount":"/data/../etc"}`, "", http.StatusBadRequest},
		{"empty", `{"mount":""}`, "", http.StatusBadRequest},
		{"null", `{"mount":null}`, "", http.StatusBadRequest},
		{"wrong type", `{"mount":42}`, "", http.StatusBadRequest},
		{"control", `{"mount":"/data\n"}`, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(`{"app_guid":"app-guid","parameters":`+tc.parameters+`}`))
			r.SetPathValue("id", testInstanceID)
			w := httptest.NewRecorder()
			b.bind(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if w.Code == http.StatusCreated {
				var response struct {
					VolumeMounts []struct {
						ContainerDir string `json:"container_dir"`
						Device       struct {
							VolumeID    string            `json:"volume_id"`
							MountConfig map[string]string `json:"mount_config"`
						} `json:"device"`
					} `json:"volume_mounts"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				mount := response.VolumeMounts[0]
				if mount.ContainerDir != tc.wantPath || mount.Device.VolumeID != testInstanceID || mount.Device.MountConfig["subdir"] != "/cf-volumes/"+testInstanceID {
					t.Fatalf("unexpected mount: %+v", mount)
				}
			}
		})
	}
}

func TestInstanceDirectoryOwnershipAndRepeatedProvision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cf-volumes", testInstanceID)
	if err := createInstanceDirectory(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0770 {
		t.Fatalf("new directory mode: %v, error: %v", info, err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := createInstanceDirectory(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0700 {
		t.Fatal("repeated provision changed existing permissions")
	}
}

func TestBrokerAuthUsesConstantShapeCredentialCheck(t *testing.T) {
	b := &broker{username: "broker", password: "secret"}
	called := false
	handler := b.auth(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/v2/catalog", nil)
	request.SetBasicAuth("broker", "secret")
	w := httptest.NewRecorder()
	handler(w, request)
	if !called || w.Code != http.StatusNoContent {
		t.Fatalf("authorized request status=%d called=%t", w.Code, called)
	}
}
