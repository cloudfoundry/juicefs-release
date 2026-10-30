package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type manifest struct {
	InstanceGroups []instanceGroup `yaml:"instance_groups"`
}

type instanceGroup struct {
	Name   string `yaml:"name"`
	VMType string `yaml:"vm_type"`
	Jobs   []job  `yaml:"jobs"`
}

type job struct {
	Name       string `yaml:"name"`
	Properties struct {
		CC struct {
			VolumeServicesEnabled *bool `yaml:"volume_services_enabled"`
		} `yaml:"cc"`
	} `yaml:"properties"`
}

func TestAcceptanceErrandUsesStandardAndInstantBoshVMTypes(t *testing.T) {
	vars := []string{"juicefs_release_version=test", "juicefs_acceptance_apps_domain=apps.example.test"}
	standard := interpolateWithVars(t, "fixtures/cf-deployment.yml", "operations/cf/enable-juicefs-acceptance-tests.yml", vars...)
	assertGroupVMType(t, standard, "juicefs-acceptance-tests", "minimal")

	instant := interpolateWithOps(t, "fixtures/instant-bosh-cf.yml", []string{
		"operations/cf/enable-juicefs-acceptance-tests.yml",
		"operations/cf/instant-bosh/acceptance-errand-vm.yml",
	}, vars...)
	assertGroupVMType(t, instant, "juicefs-acceptance-tests", "default")
}

func TestCFNonHAValidationAliasesTargetJuiceFSBackends(t *testing.T) {
	data := interpolateRaw(t, "fixtures/cf-deployment.yml", "operations/cf/non-ha-validation.yml")
	var result struct {
		Addons []struct {
			Name string `yaml:"name"`
			Jobs []struct {
				Name       string `yaml:"name"`
				Properties struct {
					Aliases []struct {
						Domain  string `yaml:"domain"`
						Targets []struct {
							InstanceGroup string `yaml:"instance_group"`
							Deployment    string `yaml:"deployment"`
						} `yaml:"targets"`
					} `yaml:"aliases"`
				} `yaml:"properties"`
			} `yaml:"jobs"`
		} `yaml:"addons"`
	}
	if err := yaml.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	var postgres, blobstore bool
	for _, addon := range result.Addons {
		for _, j := range addon.Jobs {
			for _, alias := range j.Properties.Aliases {
				if len(alias.Targets) != 1 || alias.Targets[0].InstanceGroup != "juicefs-backends" || alias.Targets[0].Deployment != "juicefs" {
					t.Errorf("alias %s targets %#v; want juicefs-backends in juicefs", alias.Domain, alias.Targets)
				}
				postgres = postgres || alias.Domain == "postgres.service.juicefs.internal"
				blobstore = blobstore || alias.Domain == "blobstore.service.juicefs.internal"
			}
		}
	}
	if !postgres || !blobstore {
		t.Errorf("validation aliases missing: postgres=%t blobstore=%t", postgres, blobstore)
	}
}

func TestCFVolumeServiceOpsTargetUpstreamGroups(t *testing.T) {
	got := interpolate(t, "fixtures/cf-deployment.yml", "operations/cf/enable-volume-services.yml")
	for _, target := range []struct{ group, jobName string }{
		{"api", "cloud_controller_ng"},
		{"cc-worker", "cloud_controller_worker"},
		{"scheduler", "cloud_controller_clock"},
	} {
		property := findJob(t, got, target.group, target.jobName).Properties.CC.VolumeServicesEnabled
		if property == nil || !*property {
			t.Errorf("%s/%s cc.volume_services_enabled = %v; want true", target.group, target.jobName, property)
		}
	}
}

func TestInstantBoshVolumeServiceOpsTargetConsolidatedGroups(t *testing.T) {
	got := interpolate(t, "fixtures/instant-bosh-cf.yml", "operations/cf/instant-bosh/enable-volume-services.yml")
	for _, jobName := range []string{"cloud_controller_ng", "cloud_controller_worker", "cloud_controller_clock"} {
		property := findJob(t, got, "control", jobName).Properties.CC.VolumeServicesEnabled
		if property == nil || !*property {
			t.Errorf("control/%s cc.volume_services_enabled = %v; want true", jobName, property)
		}
	}
}

func TestVolumeDriverRuntimeConfigTargetsCFDiegoCell(t *testing.T) {
	assertRuntimeConfigScope(t, "runtime-configs/juicefs-volume-driver.yml", "diego-cell")
}

func TestInstantBoshRuntimeConfigTargetsCompute(t *testing.T) {
	assertRuntimeConfigScope(t, "runtime-configs/instant-bosh/juicefs-volume-driver.yml", "compute")
}

func TestCFVolumeServiceOpsAgainstUpstreamManifestWhenAvailable(t *testing.T) {
	manifestPath := os.Getenv("CF_DEPLOYMENT_MANIFEST")
	if manifestPath == "" {
		t.Skip("CF_DEPLOYMENT_MANIFEST is not set; fixture-based coverage still runs")
	}
	got := interpolate(t, manifestPath, "operations/cf/enable-volume-services.yml")
	for _, target := range []struct{ group, jobName string }{
		{"api", "cloud_controller_ng"},
		{"cc-worker", "cloud_controller_worker"},
		{"scheduler", "cloud_controller_clock"},
	} {
		property := findJob(t, got, target.group, target.jobName).Properties.CC.VolumeServicesEnabled
		if property == nil || !*property {
			t.Errorf("upstream cf-deployment target %s/%s was not enabled", target.group, target.jobName)
		}
	}
}

func TestAcceptanceErrandOpsAgainstUpstreamManifestWhenAvailable(t *testing.T) {
	manifestPath := os.Getenv("CF_DEPLOYMENT_MANIFEST")
	if manifestPath == "" {
		t.Skip("CF_DEPLOYMENT_MANIFEST is not set; fixture-based coverage still runs")
	}
	got := interpolateWithVars(t, manifestPath, "operations/cf/enable-juicefs-acceptance-tests.yml",
		"juicefs_release_version=test",
		"juicefs_acceptance_apps_domain=apps.example.test",
	)
	var found bool
	for _, group := range got.InstanceGroups {
		if group.Name != "juicefs-acceptance-tests" {
			continue
		}
		found = true
		if len(group.Jobs) != 1 || group.Jobs[0].Name != "juicefs-acceptance-tests" {
			t.Errorf("acceptance group jobs = %#v; want juicefs-acceptance-tests", group.Jobs)
		}
	}
	if !found {
		t.Fatal("acceptance errand instance group not added")
	}
}

func assertRuntimeConfigScope(t *testing.T, path, group string) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "deployment", path))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Addons []struct {
			Name    string `yaml:"name"`
			Include struct {
				Deployments    []string `yaml:"deployments"`
				InstanceGroups []string `yaml:"instance_groups"`
			} `yaml:"include"`
			Jobs []job `yaml:"jobs"`
		} `yaml:"addons"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Addons) != 1 {
		t.Fatalf("got %d runtime-config addons; want one", len(config.Addons))
	}
	addon := config.Addons[0]
	if addon.Name != "juicefs-volume-driver" {
		t.Errorf("addon name = %q; want juicefs-volume-driver", addon.Name)
	}
	if !contains(addon.Include.Deployments, "cf") || !contains(addon.Include.InstanceGroups, group) {
		t.Errorf("runtime-config scope deployments=%v instance_groups=%v; want cf/%s", addon.Include.Deployments, addon.Include.InstanceGroups, group)
	}
	if len(addon.Jobs) != 1 || addon.Jobs[0].Name != "juicefs-volume-driver" {
		t.Errorf("runtime-config jobs = %#v; want juicefs-volume-driver", addon.Jobs)
	}
}

func interpolate(t *testing.T, manifestPath string, opsPath string) manifest {
	t.Helper()
	return interpolateWithVars(t, manifestPath, opsPath)
}

func interpolateWithVars(t *testing.T, manifestPath string, opsPath string, vars ...string) manifest {
	output := interpolateRaw(t, manifestPath, opsPath, vars...)
	var result manifest
	if err := yaml.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse interpolated manifest from %s (ops %s): %v\n%s", manifestPath, opsPath, err, output)
	}
	return result
}

func interpolateWithOps(t *testing.T, manifestPath string, opsPaths []string, vars ...string) manifest {
	t.Helper()
	output := interpolateRawWithOps(t, manifestPath, opsPaths, vars...)
	var result manifest
	if err := yaml.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse interpolated manifest from %s (ops %v): %v\n%s", manifestPath, opsPaths, err, output)
	}
	return result
}

func interpolateRaw(t *testing.T, manifestPath string, opsPath string, vars ...string) []byte {
	return interpolateRawWithOps(t, manifestPath, []string{opsPath}, vars...)
}

func interpolateRawWithOps(t *testing.T, manifestPath string, opsPaths []string, vars ...string) []byte {
	t.Helper()
	testsDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(testsDir, manifestPath)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"interpolate", manifestPath, "-v", "system_domain=example.test"}
	for _, opsPath := range opsPaths {
		args = append(args, "-o", filepath.Join(root, "deployment", opsPath))
	}
	for _, variable := range vars {
		args = append(args, "-v", variable)
	}
	cmd := exec.Command("bosh", args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bosh interpolate failed: %v\n%s", err, output)
	}
	return output
}

func assertGroupVMType(t *testing.T, m manifest, groupName, want string) {
	t.Helper()
	for _, group := range m.InstanceGroups {
		if group.Name == groupName {
			if group.VMType != want {
				t.Errorf("%s vm_type = %q; want %q", groupName, group.VMType, want)
			}
			return
		}
	}
	t.Errorf("instance group %q not found", groupName)
}

func findJob(t *testing.T, m manifest, groupName, jobName string) job {
	t.Helper()
	for _, group := range m.InstanceGroups {
		if group.Name != groupName {
			continue
		}
		for _, j := range group.Jobs {
			if j.Name == jobName {
				return j
			}
		}
	}
	t.Fatalf("job %s/%s not found; groups: %s", groupName, jobName, groupNames(m))
	return job{}
}

func groupNames(m manifest) string {
	var names []string
	for _, g := range m.InstanceGroups {
		names = append(names, g.Name)
	}
	return strings.Join(names, ", ")
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func repoRoot() (string, error) {
	return filepath.Abs(filepath.Join("..", ".."))
}
