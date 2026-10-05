package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	serviceID = "7c867701-b729-45d4-8a23-37cce28e1749"
	planID    = "40bf3e21-885a-4c2a-99b4-d4e8186c467e"
	driver    = "juicefs-volume-driver"
)

var instanceIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

type broker struct {
	mu              sync.Mutex
	root            string
	username        string
	password        string
	publicURL       string
	removeDirectory func(string) error
	createDirectory func(string) error
	cleanEmptyDirs  bool
}

type config struct {
	Broker struct {
		ListenAddr, Root, Username, Password, PublicURL string
		MountReadyTimeout                               int
		DeletionTimeout                                 int
		CleanEmptyDirs                                  bool
		DirectoryUID, DirectoryGID                      int
	} `json:"broker"`
}

type request struct {
	ServiceID    string                     `json:"service_id"`
	PlanID       string                     `json:"plan_id"`
	AppGUID      string                     `json:"app_guid"`
	Parameters   map[string]json.RawMessage `json:"parameters"`
	BindResource struct {
		AppGUID string `json:"app_guid"`
	} `json:"bind_resource"`
}

func main() {
	configPath := os.Getenv("BROKER_CONFIG")
	if configPath == "" {
		configPath = "/var/vcap/jobs/juicefs-broker/config/config.json"
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Broker.Root == "" || cfg.Broker.Username == "" || cfg.Broker.Password == "" {
		log.Fatal("broker root, username and password are required")
	}
	root, err := filepath.Abs(cfg.Broker.Root)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		log.Fatal(err)
	}

	b := &broker{root: root, username: cfg.Broker.Username, password: cfg.Broker.Password, publicURL: cfg.Broker.PublicURL, cleanEmptyDirs: cfg.Broker.CleanEmptyDirs}
	b.createDirectory = func(path string) error {
		return createInstanceDirectory(path, cfg.Broker.DirectoryUID, cfg.Broker.DirectoryGID)
	}
	b.removeDirectory = func(path string) error {
		if _, err := os.Lstat(path); err != nil {
			return err
		}
		return execJuiceFS("rmr", "--skip-trash", path)
	}
	mountTimeout := time.Duration(cfg.Broker.MountReadyTimeout) * time.Second
	if mountTimeout <= 0 {
		mountTimeout = 60 * time.Second
	}
	if err := waitForMount(root, mountTimeout); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v2/catalog", b.auth(b.catalog))
	mux.HandleFunc("PUT /v2/service_instances/{id}", b.auth(b.provision))
	mux.HandleFunc("DELETE /v2/service_instances/{id}", b.auth(b.deprovision))
	mux.HandleFunc("PUT /v2/service_instances/{id}/service_bindings/{binding}", b.auth(b.bind))
	mux.HandleFunc("DELETE /v2/service_instances/{id}/service_bindings/{binding}", b.auth(b.unbind))
	mux.HandleFunc("GET /v2/service_instances/{id}", b.auth(b.getInstance))

	addr := cfg.Broker.ListenAddr
	if addr == "" {
		addr = "0.0.0.0:8080"
	}
	log.Printf("starting JuiceFS service broker on %s", addr)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func waitForMount(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mounted, err := isMounted(path)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for full JuiceFS root mount")
}

func isMounted(path string) (bool, error) {
	contents, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && unescapeMountField(fields[4]) == path {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountField(field string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(field)
}

func (b *broker) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(b.username)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(b.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="service-broker"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (b *broker) catalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"services": []any{map[string]any{
		"id": serviceID, "name": "juicefs", "description": "Dynamically provisioned JuiceFS directories", "bindable": true,
		"plan_updateable": false, "requires": []string{"volume_mount"}, "tags": []string{"filesystem", "juicefs"},
		"bindings_retrievable": false,
		"plans":                []any{map[string]any{"id": planID, "name": "shared", "description": "Shared JuiceFS directory", "free": true}},
	}}})
}

func (b *broker) provision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !instanceIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid service instance id")
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ServiceID != serviceID || req.PlanID != planID {
		writeError(w, http.StatusUnprocessableEntity, "unsupported service or plan")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	root, err := b.instanceRoot()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(root, id)
	create := b.createDirectory
	if create == nil {
		create = func(path string) error { return os.MkdirAll(path, 0770) }
	}
	if err := create(path); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("create instance directory: %v", err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dashboard_url": nil})
}

func (b *broker) deprovision(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	path, err := b.instancePath(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	remove := b.removeDirectory
	if remove == nil {
		remove = removeDirectory
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("remove instance directory: %v", err))
		return
	}
	if b.cleanEmptyDirs {
		if err := removeEmptyParents(filepath.Dir(path), filepath.Join(b.root, "cf-volumes")); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("clean empty parents: %v", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func removeEmptyParents(path, stop string) error {
	path = filepath.Clean(path)
	stop = filepath.Clean(stop)
	if path == stop {
		return nil
	}
	if filepath.Dir(path) == path || !strings.HasPrefix(path, stop+string(filepath.Separator)) {
		return errors.New("parent path escapes service instance root")
	}
	for path != stop {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			path = filepath.Dir(path)
			continue
		}
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		if err != nil {
			return err
		}
		path = filepath.Dir(path)
	}
	return nil
}

func removeDirectory(path string) error {
	return os.RemoveAll(path)
}

func createInstanceDirectory(path string, uid, gid int) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0770); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0770)
}

func execJuiceFS(args ...string) error {
	cmd := exec.Command("/var/vcap/packages/juicefs/bin/juicefs", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("juicefs %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (b *broker) bind(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b.mu.Lock()
	defer b.mu.Unlock()
	path, err := b.instancePath(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, "service instance directory does not exist")
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.AppGUID == "" && req.BindResource.AppGUID == "") {
		writeError(w, http.StatusBadRequest, "app_guid is required")
		return
	}
	containerDir := filepath.Join("/var/vcap/data", id)
	if mount, ok := req.Parameters["mount"]; ok {
		var path *string
		if err := json.Unmarshal(mount, &path); err != nil || path == nil || !filepath.IsAbs(*path) || *path == "/" || filepath.Clean(*path) != *path || strings.ContainsAny(*path, "\x00\r\n\t") {
			writeError(w, http.StatusBadRequest, "mount must be a clean, absolute, non-root container path")
			return
		}
		containerDir = *path
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"credentials": map[string]any{},
		"volume_mounts": []any{map[string]any{
			"driver": driver, "container_dir": containerDir, "mode": "rw", "device_type": "shared",
			"device": map[string]any{"volume_id": id, "mount_config": map[string]string{"subdir": "/cf-volumes/" + id}},
		}},
	})
}

func (b *broker) getInstance(w http.ResponseWriter, r *http.Request) {
	path, err := b.instancePath(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "service instance directory does not exist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_id": serviceID, "plan_id": planID})
}

func serviceInstanceExists(root, id string) bool {
	_, err := os.Stat(filepath.Join(root, id))
	return err == nil
}

func (b *broker) unbind(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (b *broker) instancePath(id string) (string, error) {
	if !instanceIDPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", errors.New("invalid service instance id")
	}
	root, err := b.instanceRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, id)
	if filepath.Dir(path) != root {
		return "", errors.New("service instance path escapes volume root")
	}
	return path, nil
}

func (b *broker) instanceRoot() (string, error) {
	root := filepath.Join(b.root, "cf-volumes")
	if filepath.Dir(root) != b.root || strings.TrimSpace(b.root) == "/" {
		return "", errors.New("invalid JuiceFS mount root")
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("service instance root must be a real directory")
	}
	return root, nil
}

func writeError(w http.ResponseWriter, status int, description string) {
	writeJSON(w, status, map[string]string{"error": "juicefs-broker-error", "description": description})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
