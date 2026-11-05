package acceptance_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
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
	for _, scenario := range []struct {
		name         string
		losesFile    bool
		stagingFails bool
		logsFail     bool
	}{
		{name: "successful persistence"},
		{name: "file lost on restart", losesFile: true},
		{name: "staging failure", stagingFails: true},
		{name: "log cache unavailable", losesFile: true, logsFail: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var mu sync.Mutex
			var stored, identity string
			var deleted []string
			var uploaded, restarted bool
			var diagnostics []string
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
					respond(map[string]any{"links": map[string]any{"uaa": map[string]string{"href": endpoint}, "login": map[string]string{"href": endpoint}, "log_cache": map[string]string{"href": endpoint}}})
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
					if scenario.stagingFails && r.Method == "GET" {
						respond(map[string]any{"guid": "build", "state": "FAILED", "error": "fixture staging failure"})
						break
					}
					respond(map[string]any{"guid": "build", "state": "STAGED", "droplet": map[string]string{"guid": "droplet"}})
				case "/v3/apps/app/processes/web/stats":
					if (!scenario.stagingFails && (len(deleted) != 1 || deleted[0] != "/v3/routes/route")) || (scenario.stagingFails && len(deleted) != 0) {
						t.Error("diagnostics must run before app and service cleanup")
					}
					diagnostics = append(diagnostics, "stats")
					respond(map[string]any{"resources": []any{}})
				case "/api/v1/read/app":
					if r.Header.Get("Authorization") != "Bearer fixture-token" {
						t.Error("log request is not authenticated")
					}
					diagnostics = append(diagnostics, "logs")
					if scenario.logsFail {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					respond(map[string]any{"envelopes": map[string]any{"batch": []any{
						map[string]any{"timestamp": "123", "tags": map[string]string{"source_type": "STG"}, "log": map[string]string{"payload": base64.StdEncoding.EncodeToString([]byte("fixture staging log"))}},
					}}})
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
					if scenario.losesFile {
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
			if (err != nil) != (scenario.losesFile || scenario.stagingFails) {
				t.Fatalf("unexpected suite outcome: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			if !uploaded || (!scenario.stagingFails && !restarted) {
				t.Fatalf("incomplete lifecycle\n%s", output)
			}
			want := []string{"/v3/routes/route", "/v3/apps/app", "/v3/service_instances/instance", "/v3/spaces/space", "/v3/organizations/org"}
			if scenario.stagingFails {
				want = want[1:]
			}
			if !reflect.DeepEqual(deleted, want) {
				t.Fatalf("cleanup order: got %v, want %v\n%s", deleted, want, output)
			}
			if scenario.losesFile && !strings.Contains(string(output), "retrieving the same GUID") {
				t.Fatalf("failed before persistence check\n%s", output)
			}
			if scenario.losesFile || scenario.stagingFails {
				wantLog := "fixture staging log"
				if scenario.logsFail {
					wantLog = "Recent app logs unavailable: cfclient: HTTP error (503)"
				}
				wantBuild := "Build build state: STAGED"
				if scenario.stagingFails {
					wantBuild = "Build error: fixture staging failure"
				}
				if !reflect.DeepEqual(diagnostics, []string{"stats", "logs"}) || !strings.Contains(string(output), wantLog) || !strings.Contains(string(output), wantBuild) {
					t.Fatalf("missing failure diagnostics: %v\n%s", diagnostics, output)
				}
			} else if len(diagnostics) != 0 {
				t.Fatalf("collected failure diagnostics for a passing spec: %v", diagnostics)
			}
		})
	}
}
