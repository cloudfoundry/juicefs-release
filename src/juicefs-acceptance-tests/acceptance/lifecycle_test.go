package acceptance_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Exercise the real suite in a subprocess against a CAPI/UAA/router fixture.
// This verifies orchestration and cleanup even when persistence verification fails.
func TestMockCFLifecycle(t *testing.T) {
	if os.Getenv("JUICEFS_ACCEPTANCE_CONFIG") != "" {
		t.Skip("fixture only runs outside the live acceptance errand")
	}
	for _, losesFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("loses-file-on-restart=%t", losesFile), func(t *testing.T) {
			var mu sync.Mutex
			var stored, identity string
			var deleted []string
			var uploaded, restarted bool
			identity = "process-one"
			var endpoint string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
				object := func(guid string) { respond(map[string]any{"guid": guid}) }
				list := func(guid string) {
					respond(map[string]any{"pagination": map[string]any{"total_results": 1, "total_pages": 1}, "resources": []map[string]string{{"guid": guid}}})
				}
				if r.Method == "DELETE" {
					deleted = append(deleted, r.URL.Path)
					w.WriteHeader(202)
					return
				}
				switch r.URL.Path {
				case "/", "/v3/info":
					respond(map[string]any{"links": map[string]any{"uaa": map[string]string{"href": endpoint}, "login": map[string]string{"href": endpoint}}})
				case "/oauth/token":
					respond(map[string]any{"access_token": "fixture-token", "token_type": "bearer", "expires_in": 3600})
				case "/v3/service_brokers":
					if r.URL.Query().Get("names") != "juicefs" {
						t.Error("broker name filter missing")
					}
					list("broker")
				case "/v3/service_offerings":
					if r.URL.Query().Get("service_broker_guids") != "broker" {
						t.Error("offering not scoped to broker")
					}
					list("offering")
				case "/v3/service_plans":
					if r.URL.Query().Get("service_offering_guids") != "offering" || r.URL.Query().Get("names") != "shared" {
						t.Error("plan filters missing")
					}
					list("plan")
				case "/v3/organizations":
					object("org")
				case "/v3/spaces":
					object("space")
				case "/v3/service_instances":
					if r.Method == "POST" {
						w.Header().Set("Location", endpoint+"/v3/jobs/provision")
						w.WriteHeader(202)
					} else {
						list("instance")
					}
				case "/v3/jobs/provision":
					respond(map[string]string{"state": "COMPLETE"})
				case "/v3/apps":
					object("app")
				case "/v3/service_credential_bindings":
					var binding struct {
						Parameters map[string]string `json:"parameters"`
					}
					if err := json.NewDecoder(r.Body).Decode(&binding); err != nil || binding.Parameters["mount"] != "/data" {
						t.Error("binding must request /data")
					}
					object("binding")
				case "/v3/packages":
					respond(map[string]any{"guid": "package", "type": "bits", "state": "AWAITING_UPLOAD", "data": map[string]any{}})
				case "/v3/packages/package/upload":
					if err := r.ParseMultipartForm(8 << 20); err != nil {
						t.Error(err)
					}
					bits, _, err := r.FormFile("bits")
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer bits.Close()
					data, err := io.ReadAll(bits)
					if err != nil {
						t.Error(err)
					}
					archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(archive.File) != 2 || archive.File[0].Name != "test-app" || archive.File[0].Mode().Perm() != 0755 {
						t.Error("missing executable app in upload")
					}
					uploaded = true
					respond(map[string]any{"guid": "package", "type": "bits", "state": "PROCESSING_UPLOAD", "data": map[string]any{}})
				case "/v3/packages/package":
					respond(map[string]any{"guid": "package", "type": "bits", "state": "READY", "data": map[string]any{}})
				case "/v3/builds", "/v3/builds/build":
					respond(map[string]any{"guid": "build", "state": "STAGED", "droplet": map[string]string{"guid": "droplet"}})
				case "/v3/apps/app/relationships/current_droplet":
					respond(map[string]any{"data": map[string]string{"guid": "droplet"}})
				case "/v3/domains":
					list("domain")
				case "/v3/routes":
					respond(map[string]string{"guid": "route", "url": strings.TrimPrefix(endpoint, "https://")})
				case "/v3/routes/route/destinations":
					respond(map[string]any{"destinations": []any{}})
				case "/v3/apps/app/processes":
					list("web")
				case "/v3/processes/web/actions/scale":
					object("web")
				case "/v3/apps/app/actions/start":
					object("app")
				case "/v3/apps/app/actions/restart":
					identity = "process-two"
					restarted = true
					if losesFile {
						stored = ""
					}
					object("app")
				case "/identity":
					_, _ = io.WriteString(w, identity)
				case "/file":
					if r.Method == "POST" {
						body, _ := io.ReadAll(r.Body)
						stored = string(body)
						w.WriteHeader(204)
					} else if stored == "" {
						w.WriteHeader(404)
					} else {
						_, _ = io.WriteString(w, stored)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			endpoint = server.URL
			dir := t.TempDir()
			binary := filepath.Join(dir, "test-app")
			if err := os.WriteFile(binary, []byte("fixture binary"), 0755); err != nil {
				t.Fatal(err)
			}
			cfg, err := json.Marshal(settings{APIURL: endpoint, Username: "admin", Password: "secret", BrokerName: "juicefs", PlanName: "shared", AppsDomain: "example.test", AppBinary: binary, SkipSSLValidation: true})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, cfg, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestAcceptance$", "-test.timeout=1m")
			command.Env = append(os.Environ(), "JUICEFS_ACCEPTANCE_CONFIG="+path)
			output, err := command.CombinedOutput()
			if (err != nil) != losesFile {
				t.Fatalf("unexpected suite outcome: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			if !uploaded || !restarted {
				t.Fatalf("incomplete lifecycle\n%s", output)
			}
			want := []string{"/v3/routes/route", "/v3/apps/app", "/v3/service_instances/instance", "/v3/spaces/space", "/v3/organizations/org"}
			if !reflect.DeepEqual(deleted, want) {
				t.Fatalf("cleanup order: got %v, want %v\n%s", deleted, want, output)
			}
			if losesFile && !strings.Contains(string(output), "retrieving the same GUID") {
				t.Fatalf("failed before persistence check\n%s", output)
			}
		})
	}
}
