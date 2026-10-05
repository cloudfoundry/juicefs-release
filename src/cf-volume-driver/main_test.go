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

const (
	testVolumeID = "6a8cc1ed-a799-43c8-8ea2-38c4cf14fb19"
	testSubdir   = "/cf-volumes/" + testVolumeID
)

func TestCreateRejectsAnySubdirOutsideInstancePrefix(t *testing.T) {
	d := newTestDriver(t)
	for _, subdir := range []string{"/", "/cf-volumes/other", "/cf-volumes/../secret", "/cf-volumes/" + testVolumeID + "/nested"} {
		body, _ := json.Marshal(createRequest{Name: testVolumeID, Opts: map[string]any{"subdir": subdir}})
		w := httptest.NewRecorder()
		d.handleCreate(w, httptest.NewRequest(http.MethodPost, "/VolumeDriver.Create", bytes.NewReader(body)))
		var response errorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		if response.Err == "" {
			t.Errorf("subdir %q accepted", subdir)
		}
	}
}

func TestCreateMountConfigContainsOnlyTheValidatedSubdir(t *testing.T) {
	d := newTestDriver(t)
	body, _ := json.Marshal(createRequest{Name: testVolumeID, Opts: map[string]any{"subdir": testSubdir}})
	w := httptest.NewRecorder()
	d.handleCreate(w, httptest.NewRequest(http.MethodPost, "/VolumeDriver.Create", bytes.NewReader(body)))
	var response errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Err != "" {
		t.Fatalf("create response: %s (%v)", w.Body.String(), err)
	}
	if d.volumes[testVolumeID] == nil || d.volumes[testVolumeID].Subdir != testSubdir {
		t.Fatal("driver did not store instance-specific subdir")
	}
	if err := d.writeMountConfig(testVolumeID, testSubdir, filepath.Join(d.mountRoot, testVolumeID), filepath.Join(d.cacheRoot, testVolumeID)); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(d.mountRoot, testVolumeID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(contents, &config); err != nil {
		t.Fatal(err)
	}
	volumeConfig := config["volume"].(map[string]any)
	if volumeConfig["subdir"] != testSubdir {
		t.Fatalf("mounted subdir %v, want %s", volumeConfig["subdir"], testSubdir)
	}
}

func TestRemoveRejectsMountedVolume(t *testing.T) {
	d := newTestDriver(t)
	d.volumes[testVolumeID] = &volume{Subdir: testSubdir, MountCount: 1}
	body, _ := json.Marshal(nameRequest{Name: testVolumeID})
	w := httptest.NewRecorder()
	d.handleRemove(w, httptest.NewRequest(http.MethodPost, "/VolumeDriver.Remove", bytes.NewReader(body)))
	var response errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if response.Err == "" {
		t.Fatal("remove accepted a mounted volume")
	}
}

func newTestDriver(t *testing.T) *driver {
	t.Helper()
	root := t.TempDir()
	return &driver{
		volumes:    map[string]*volume{},
		statePath:  filepath.Join(root, "state", "driver.json"),
		configPath: filepath.Join(root, "base.json"),
		mountRoot:  filepath.Join(root, "mounts"),
		cacheRoot:  filepath.Join(root, "cache"),
		baseConfig: map[string]any{"postgres": map[string]any{"host": "metadata"}, "volume": map[string]any{"name": "volume", "subdir": ""}},
	}
}
