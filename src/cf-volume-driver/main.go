package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	driverName = "juicefs-volume-driver"
	serviceBin = "/var/vcap/packages/juicefs/bin/juicefs-service"
)

type volume struct {
	Subdir     string `json:"subdir"`
	Mountpoint string `json:"mountpoint,omitempty"`
	MountCount int    `json:"mount_count,omitempty"`
	cmd        *exec.Cmd
	done       chan error
}

type driver struct {
	mu         sync.Mutex
	volumes    map[string]*volume
	statePath  string
	configPath string
	mountRoot  string
	cacheRoot  string
	baseConfig map[string]any
	stop       chan struct{}
}

type createRequest struct {
	Name string         `json:"Name"`
	Opts map[string]any `json:"Opts"`
}

type nameRequest struct {
	Name string `json:"Name"`
}

type mountResponse struct {
	Err        string `json:"Err"`
	Mountpoint string `json:"Mountpoint,omitempty"`
}

type errorResponse struct {
	Err string `json:"Err"`
}

type activateResponse struct {
	Implements []string `json:"Implements"`
}

type volumeInfo struct {
	Name       string `json:"Name"`
	Mountpoint string `json:"Mountpoint,omitempty"`
	MountCount int    `json:"MountCount,omitempty"`
}

type listResponse struct {
	Err     string       `json:"Err"`
	Volumes []volumeInfo `json:"Volumes"`
}

type getResponse struct {
	Err    string     `json:"Err"`
	Volume volumeInfo `json:"Volume"`
}

func main() {
	baseConfigPath := os.Getenv("JUICEFS_DRIVER_CONFIG")
	if len(os.Args) == 5 && os.Args[1] == "--mount" {
		if err := mountWithOverrides(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) != 1 {
		log.Fatalf("usage: cf-volume-driver [--mount CONFIG.json MOUNTPOINT CACHEDIR]")
	}
	if baseConfigPath == "" {
		baseConfigPath = "/var/vcap/jobs/juicefs-volume-driver/config/config.json"
	}
	baseContents, err := os.ReadFile(baseConfigPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read driver config:", err)
		os.Exit(1)
	}
	var baseConfig map[string]any
	if err := json.Unmarshal(baseContents, &baseConfig); err != nil {
		fmt.Fprintln(os.Stderr, "parse driver config:", err)
		os.Exit(1)
	}
	volumeConfig, ok := baseConfig["volume"].(map[string]any)
	if !ok {
		fmt.Fprintln(os.Stderr, "driver config has no volume object")
		os.Exit(1)
	}
	mountRoot, _ := volumeConfig["mountPoint"].(string)
	cacheRoot, _ := volumeConfig["cacheDir"].(string)
	if mountRoot == "" || cacheRoot == "" {
		fmt.Fprintln(os.Stderr, "driver mount and cache paths are required")
		os.Exit(1)
	}
	d := &driver{
		volumes:    map[string]*volume{},
		statePath:  "/var/vcap/data/juicefs-volume-driver/state.json",
		configPath: baseConfigPath,
		mountRoot:  mountRoot,
		cacheRoot:  cacheRoot,
		baseConfig: baseConfig,
	}
	proxyCmd := exec.Command("/var/vcap/packages/juicefs/bin/juicefs-service", "proxy", baseConfigPath)
	proxyCmd.Stdout, proxyCmd.Stderr = os.Stdout, os.Stderr
	if err := proxyCmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start WebDAV proxy:", err)
		os.Exit(1)
	}
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxyCmd.Wait() }()
	if err := waitForProxy(proxyDone, 15*time.Second); err != nil {
		shutdownProxy(proxyCmd, proxyDone)
		log.Fatal("start WebDAV proxy:", err)
	}
	if err := d.load(); err != nil {
		shutdownProxy(proxyCmd, proxyDone)
		log.Fatal("restore driver state:", err)
	}
	if err := d.register(); err != nil {
		shutdownProxy(proxyCmd, proxyDone)
		log.Fatal("register driver:", err)
	}
	if err := d.recoverMounts(); err != nil {
		shutdownProxy(proxyCmd, proxyDone)
		log.Fatal("recover mounts:", err)
	}
	go func() {
		for range time.Tick(time.Second) {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:19798", 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				continue
			}
			log.Printf("WebDAV proxy unavailable: %v", err)
			if err := proxyCmd.Process.Signal(syscall.SIGTERM); err == nil {
				select {
				case <-proxyDone:
				case <-time.After(2 * time.Second):
					_ = proxyCmd.Process.Kill()
					<-proxyDone
				}
			}
			os.Exit(1)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /Plugin.Activate", func(w http.ResponseWriter, _ *http.Request) {
		write(w, activateResponse{Implements: []string{"VolumeDriver"}})
	})
	mux.HandleFunc("POST /VolumeDriver.Create", d.handleCreate)
	mux.HandleFunc("POST /VolumeDriver.Remove", d.handleRemove)
	mux.HandleFunc("POST /VolumeDriver.Mount", d.handleMount)
	mux.HandleFunc("POST /VolumeDriver.Unmount", d.handleUnmount)
	mux.HandleFunc("POST /VolumeDriver.Get", d.handleGet)
	mux.HandleFunc("POST /VolumeDriver.Path", d.handlePath)
	mux.HandleFunc("POST /VolumeDriver.List", d.handleList)
	mux.HandleFunc("POST /VolumeDriver.Capabilities", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"Capabilities": map[string]string{"Scope": "local"}})
	})

	addr := os.Getenv("JUICEFS_DRIVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7591"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	d.stop = make(chan struct{})
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err = <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case <-signals:
		close(d.stop)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
	}
	d.unmountAll()
	shutdownProxy(proxyCmd, proxyDone)
}

func mountWithOverrides(configPath, mountpoint, cacheDir string) error {
	path, cleanup, err := writeMountOverride(configPath, mountpoint, cacheDir)
	if err != nil {
		return err
	}
	defer cleanup()
	return runJuiceFSMount(path)
}

func writeMountOverride(configPath, mountpoint, cacheDir string) (string, func(), error) {
	contents, err := os.ReadFile(configPath)
	if err != nil {
		return "", nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(contents, &config); err != nil {
		return "", nil, err
	}
	volumeConfig, ok := config["volume"].(map[string]any)
	if !ok {
		return "", nil, errors.New("mount config has no volume object")
	}
	volumeConfig["mountPoint"], volumeConfig["cacheDir"] = mountpoint, cacheDir
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp("", "juicefs-mount-*.json")
	if err != nil {
		return "", nil, err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, err
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

func runJuiceFSMount(configPath string) error {
	cmd := exec.Command(serviceBin, "mount", configPath)
	cmd.Env = os.Environ()
	return runCommandUntilSignal(cmd)
}

func runCommandUntilSignal(cmd *exec.Cmd) error {
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case sig := <-signals:
		_ = cmd.Process.Signal(sig)
		return <-done
	}
}

func waitForProxy(done <-chan error, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:19798", 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case err := <-done:
			if err == nil {
				return errors.New("WebDAV proxy exited unexpectedly")
			}
			return fmt.Errorf("WebDAV proxy exited: %w", err)
		case <-deadline.C:
			return errors.New("timed out waiting for WebDAV proxy")
		case <-ticker.C:
		}
	}
}

func shutdownProxy(cmd *exec.Cmd, done <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func (d *driver) register() error {
	const dir = "/var/vcap/data/voldrivers"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	spec, err := json.Marshal(map[string]any{"Name": driverName, "Addr": "http://127.0.0.1:7591", "UniqueVolumeIds": false})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, driverName+".json"), spec, 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, driverName+".spec"), []byte("http://127.0.0.1:7591"), 0644)
}

func (d *driver) load() error {
	if err := os.MkdirAll(filepath.Dir(d.statePath), 0750); err != nil {
		return err
	}
	contents, err := os.ReadFile(d.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var persisted map[string]string
	if err := json.Unmarshal(contents, &persisted); err != nil {
		return err
	}
	for id, subdir := range persisted {
		if !validID(id) || !validSubdir(subdir, id) {
			continue
		}
		d.volumes[id] = &volume{Subdir: subdir}
	}
	return nil
}

func (d *driver) recoverMounts() error {
	for id, v := range d.volumes {
		mounted, err := isMounted(filepath.Join(d.mountRoot, id))
		if err != nil {
			return err
		}
		if mounted {
			// A cell reboot or driver restart may leave mountinfo state while the
			// old process is gone. Do not invent a live refcount; future Mount
			// calls below will create a fresh foreground client after cleanup.
			if err := syscall.Unmount(filepath.Join(d.mountRoot, id), syscall.MNT_DETACH); err != nil {
				return fmt.Errorf("recover stale JuiceFS mount %s: %w", id, err)
			}
		}
		v.MountCount = 0
		v.Mountpoint = ""
	}
	return d.save()
}

func (d *driver) save() error {
	if err := os.MkdirAll(filepath.Dir(d.statePath), 0750); err != nil {
		return err
	}
	persisted := make(map[string]string, len(d.volumes))
	for id, v := range d.volumes {
		persisted[id] = v.Subdir
	}
	contents, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	tmp := d.statePath + ".tmp"
	if err := os.WriteFile(tmp, contents, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, d.statePath)
}

func (d *driver) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, errorResponse{Err: "invalid volume id"})
		return
	}
	subdir, ok := req.Opts["subdir"].(string)
	if !ok || !validSubdir(subdir, req.Name) {
		write(w, errorResponse{Err: "mount_config must contain a subdir matching the volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing := d.volumes[req.Name]; existing != nil && existing.Subdir != subdir {
		write(w, errorResponse{Err: "volume id already has different configuration"})
		return
	}
	if d.volumes[req.Name] == nil {
		d.volumes[req.Name] = &volume{Subdir: subdir}
	}
	if err := d.save(); err != nil {
		write(w, errorResponse{Err: err.Error()})
		return
	}
	write(w, errorResponse{})
}

func (d *driver) handleMount(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, mountResponse{Err: "invalid volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.volumes[req.Name]
	if v == nil {
		write(w, mountResponse{Err: "volume must be created before it is mounted"})
		return
	}
	if v.MountCount > 0 && (v.cmd == nil || v.cmd.Process == nil) {
		write(w, mountResponse{Err: "volume has positive reference count but no mount process; unmount before retrying"})
		return
	}
	if v.MountCount > 0 && v.cmd != nil && v.cmd.Process != nil {
		select {
		case err := <-v.done:
			v.cmd = nil
			v.done = nil
			if err != nil {
				write(w, mountResponse{Err: fmt.Sprintf("previous mount exited: %v", err)})
				return
			}
		default:
			v.MountCount++
			if err := d.save(); err != nil {
				v.MountCount--
				write(w, mountResponse{Err: err.Error()})
				return
			}
			write(w, mountResponse{Mountpoint: v.Mountpoint})
			return
		}
	}

	mountpoint := filepath.Join(d.mountRoot, req.Name)
	cache := filepath.Join(d.cacheRoot, req.Name)
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		write(w, mountResponse{Err: err.Error()})
		return
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		write(w, mountResponse{Err: err.Error()})
		return
	}
	if err := d.writeMountConfig(req.Name, v.Subdir, mountpoint, cache); err != nil {
		write(w, mountResponse{Err: err.Error()})
		return
	}
	configPath, cleanupConfig, err := writeMountOverride(filepath.Join(d.mountRoot, req.Name+".json"), mountpoint, cache)
	if err != nil {
		write(w, mountResponse{Err: err.Error()})
		return
	}
	cmd := exec.Command("/var/vcap/packages/juicefs/bin/juicefs-service", "mount", configPath)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		cleanupConfig()
		write(w, mountResponse{Err: err.Error()})
		return
	}
	v.cmd = cmd
	v.done = make(chan error, 1)
	go func(done chan error) {
		done <- cmd.Wait()
		cleanupConfig()
	}(v.done)
	if err := d.waitMounted(mountpoint, v.done, 15*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		write(w, mountResponse{Err: err.Error()})
		return
	}
	v.Mountpoint, v.MountCount = mountpoint, 1
	if err := d.save(); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		v.MountCount = 0
		write(w, mountResponse{Err: err.Error()})
		return
	}
	write(w, mountResponse{Mountpoint: mountpoint})
}

func (d *driver) handleUnmount(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, errorResponse{Err: "invalid volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.volumes[req.Name]
	if v == nil {
		write(w, errorResponse{})
		return
	}
	if v.MountCount <= 0 {
		write(w, errorResponse{})
		return
	}
	if v.MountCount > 1 {
		v.MountCount--
		if err := d.save(); err != nil {
			v.MountCount++
			write(w, errorResponse{Err: err.Error()})
			return
		}
		write(w, errorResponse{})
		return
	}
	if v.cmd != nil && v.cmd.Process != nil {
		_ = v.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-v.done:
		case <-time.After(30 * time.Second):
			_ = v.cmd.Process.Kill()
			<-v.done
		}
	}
	v.cmd, v.done, v.MountCount, v.Mountpoint = nil, nil, 0, ""
	if err := d.save(); err != nil {
		write(w, errorResponse{Err: err.Error()})
		return
	}
	write(w, errorResponse{})
}

func (d *driver) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, errorResponse{Err: "invalid volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.volumes[req.Name]
	if v == nil {
		write(w, errorResponse{})
		return
	}
	if v.MountCount > 0 {
		write(w, errorResponse{Err: "cannot remove a mounted volume"})
		return
	}
	delete(d.volumes, req.Name)
	if err := d.save(); err != nil {
		d.volumes[req.Name] = v
		write(w, errorResponse{Err: err.Error()})
		return
	}
	write(w, errorResponse{})
}

func (d *driver) unmountAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range d.volumes {
		if v.cmd == nil || v.cmd.Process == nil {
			continue
		}
		_ = v.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-v.done:
		case <-time.After(30 * time.Second):
			_ = v.cmd.Process.Kill()
			<-v.done
		}
		v.cmd, v.done, v.MountCount, v.Mountpoint = nil, nil, 0, ""
	}
	_ = d.save()
}

func (d *driver) handleGet(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, getResponse{Err: "invalid volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.volumes[req.Name]
	if v == nil {
		write(w, getResponse{Err: "volume not found"})
		return
	}
	write(w, getResponse{Volume: volumeInfo{Name: req.Name, Mountpoint: v.Mountpoint, MountCount: v.MountCount}})
}

func (d *driver) handlePath(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validID(req.Name) {
		write(w, mountResponse{Err: "invalid volume id"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.volumes[req.Name]
	if v == nil || v.Mountpoint == "" {
		write(w, mountResponse{Err: "volume is not mounted"})
		return
	}
	write(w, mountResponse{Mountpoint: v.Mountpoint})
}

func (d *driver) handleList(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	resp := listResponse{Volumes: []volumeInfo{}}
	for id, v := range d.volumes {
		resp.Volumes = append(resp.Volumes, volumeInfo{Name: id, Mountpoint: v.Mountpoint, MountCount: v.MountCount})
	}
	write(w, resp)
}

func (d *driver) writeMountConfig(id, subdir, mountpoint, cache string) error {
	if err := os.MkdirAll(d.mountRoot, 0750); err != nil {
		return err
	}
	config := make(map[string]any, len(d.baseConfig))
	for key, value := range d.baseConfig {
		config[key] = value
	}
	baseVolumeConfig, ok := d.baseConfig["volume"].(map[string]any)
	if !ok {
		return errors.New("base JuiceFS config has no volume object")
	}
	volumeConfig := make(map[string]any, len(baseVolumeConfig))
	for key, value := range baseVolumeConfig {
		volumeConfig[key] = value
	}
	config["volume"] = volumeConfig
	volumeConfig["subdir"] = subdir
	volumeConfig["mountPoint"] = mountpoint
	volumeConfig["cacheDir"] = cache
	path := filepath.Join(d.mountRoot, id+".json")
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0600)
}

func (d *driver) waitMounted(mountpoint string, done <-chan error, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		mounted, err := isMounted(mountpoint)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
		select {
		case <-d.stop:
			return errors.New("driver shutting down")
		case <-d.stop:
			return errors.New("driver shutting down")
		case err := <-done:
			if err == nil {
				return errors.New("JuiceFS mount process exited before mounting")
			}
			return fmt.Errorf("JuiceFS mount failed: %w", err)
		case <-deadline.C:
			return errors.New("timed out waiting for JuiceFS mount")
		case <-ticker.C:
		}
	}
}

func isMounted(mountpoint string) (bool, error) {
	contents, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == mountpoint {
			return true, nil
		}
	}
	return false, nil
}

func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validSubdir(subdir, id string) bool {
	return subdir == "/cf-volumes/"+id
}

func write(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
