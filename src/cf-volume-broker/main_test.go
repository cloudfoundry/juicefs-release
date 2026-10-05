package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testInstanceID = "6a8cc1ed-a799-43c8-8ea2-38c4cf14fb19"

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
