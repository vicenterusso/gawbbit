// Package ansible holds public-safe contract tests for the VM-two Ansible
// role that drives this deployer (ported from tests/test_ansible_dbt_deploy.py).
package ansible

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	repo = "../../../../.."
	role = repo + "/quickstart/ansible/roles/rawbbit_two"
)

type task = map[string]any

func loadYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// tasks flattens top-level tasks and their block/rescue/always children.
func tasks(t *testing.T, path string) []task {
	var top []task
	loadYAML(t, path, &top)
	var out []task
	for _, item := range top {
		out = append(out, item)
		for _, key := range []string{"block", "rescue", "always"} {
			children, _ := item[key].([]any)
			for _, child := range children {
				if c, ok := child.(task); ok {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func name(tk task) string {
	s, _ := tk["name"].(string)
	return s
}

func index(t *testing.T, list []task, match func(task) bool, what string) int {
	t.Helper()
	for i, tk := range list {
		if match(tk) {
			return i
		}
	}
	t.Fatalf("task not found: %s", what)
	return -1
}

func sub(tk task, key string) map[string]any {
	m, _ := tk[key].(map[string]any)
	return m
}

func TestLocalImageIsAlwaysRebuiltBeforeCandidateActivation(t *testing.T) {
	main := tasks(t, role+"/tasks/main.yml")
	buildIndex := index(t, main, func(tk task) bool { _, ok := tk["community.docker.docker_image_build"]; return ok }, "image build")
	copyIndex := index(t, main, func(tk task) bool { return name(tk) == "Copy dbt-runner build context for the local image" }, "copy context")
	validateIndex := index(t, main, func(tk task) bool { return strings.HasPrefix(name(tk), "Validate staged routing") }, "validate")
	activationIndex := index(t, main, func(tk task) bool { return tk["ansible.builtin.include_tasks"] == "activate-dbt-runner.yml" }, "activate")
	buildTask := main[buildIndex]
	build := sub(buildTask, "community.docker.docker_image_build")
	if build["rebuild"] != "always" || build["name"] != "{{ rawbbit_two_dbt_runner_image }}" ||
		build["path"] != "{{ rawbbit_two_dbt_build_context }}" {
		t.Errorf("build task %v", build)
	}
	if !reflect.DeepEqual(build["args"], map[string]any{"DBT_UID": "{{ rawbbit_two_deploy_uid.stdout }}",
		"DBT_GID": "{{ rawbbit_two_deploy_gid.stdout }}"}) {
		t.Errorf("build args %v", build["args"])
	}
	if buildTask["become"] != false {
		t.Error("image build must not become root")
	}
	for _, tk := range []task{main[copyIndex], buildTask} {
		if tk["when"] != `rawbbit_two_dbt_runner_image == "rawbbit-dbt-runner:local"` {
			t.Errorf("%s: when %v", name(tk), tk["when"])
		}
	}
	cp := sub(main[copyIndex], "ansible.builtin.copy")
	if cp["src"] != "{{ playbook_dir }}/../../dbt_project/" || cp["dest"] != "{{ rawbbit_two_dbt_build_context }}/" {
		t.Errorf("copy %v", cp)
	}
	if v, _ := build["nocache"].(bool); v {
		t.Error("nocache")
	}
	if v, _ := build["pull"].(bool); v {
		t.Error("pull")
	}
	if v, _ := buildTask["ignore_errors"].(bool); v {
		t.Error("ignore_errors")
	}
	if !(copyIndex < buildIndex && buildIndex < validateIndex && validateIndex < activationIndex) {
		t.Errorf("order copy=%d build=%d validate=%d activate=%d", copyIndex, buildIndex, validateIndex, activationIndex)
	}
}

func TestEveryStackActionIsScopedAndCannotRecreateRunner(t *testing.T) {
	paths, _ := filepath.Glob(role + "/tasks/*.yml")
	if len(paths) == 0 {
		t.Fatal("no role tasks")
	}
	for _, path := range paths {
		for _, tk := range tasks(t, path) {
			compose := sub(tk, "community.docker.docker_compose_v2")
			if compose == nil {
				continue
			}
			services, _ := compose["services"].([]any)
			if len(services) == 0 {
				t.Errorf("%s/%s: unscoped compose action", filepath.Base(path), name(tk))
			}
			for _, s := range services {
				if s == "dbt-runner" {
					t.Errorf("%s/%s: compose action targets dbt-runner", filepath.Base(path), name(tk))
				}
			}
			if compose["state"] == "present" {
				if compose["dependencies"] != false {
					t.Errorf("%s/%s: dependencies not disabled", filepath.Base(path), name(tk))
				}
				for _, s := range services {
					if s == "clickhouse" && (!reflect.DeepEqual(services, []any{"clickhouse"}) || compose["recreate"] != "never") {
						t.Errorf("%s/%s: clickhouse may be recreated", filepath.Base(path), name(tk))
					}
				}
			}
		}
	}
}

func TestEnvironmentAndComposeAreStagedUntilHostTransaction(t *testing.T) {
	main := tasks(t, role+"/tasks/main.yml")
	env := main[index(t, main, func(tk task) bool { return sub(tk, "ansible.builtin.template")["src"] == "env.j2" }, "env template")]
	if dest, _ := sub(env, "ansible.builtin.template")["dest"].(string); !strings.Contains(dest, "rawbbit_two_stage.path") {
		t.Errorf("env rendered outside stage: %s", dest)
	}
	cp := main[index(t, main, func(tk task) bool { return name(tk) == "Copy required VM-two runtime artifacts" }, "artifacts copy")]
	loop, _ := cp["loop"].([]any)
	for _, item := range loop {
		if p, _ := item.(map[string]any)["path"].(string); strings.HasPrefix(p, "docker-compose") {
			t.Errorf("compose file %s copied before host transaction", p)
		}
	}
	if force, _ := sub(cp, "ansible.builtin.copy")["force"].(string); !strings.Contains(force, "clickhouse/") {
		t.Errorf("force expression %q", force)
	}
	validate := index(t, main, func(tk task) bool { return strings.HasPrefix(name(tk), "Validate staged routing") }, "validate")
	provision := index(t, main, func(tk task) bool { return strings.HasPrefix(name(tk), "Reconcile managed datasets") }, "provision")
	runner := index(t, main, func(tk task) bool { return tk["ansible.builtin.include_tasks"] == "activate-dbt-runner.yml" }, "runner")
	activation := index(t, main, func(tk task) bool { return strings.HasPrefix(name(tk), "Activate and deploy") }, "activation")
	if !(validate < provision && runner < activation) {
		t.Errorf("order validate=%d provision=%d runner=%d activation=%d", validate, provision, runner, activation)
	}
}

func TestNoRouteOverlayOrProtectedRegistryMountedIntoRunner(t *testing.T) {
	var compose struct {
		Services map[string]struct {
			Volumes     []string          `yaml:"volumes"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	loadYAML(t, repo+"/quickstart/vm_rawbbit_two/docker-compose.yml", &compose)
	runner := compose.Services["dbt-runner"]
	if !reflect.DeepEqual(runner.Volumes, []string{"/srv/rawbbit-two/dbt:/app/runtime"}) {
		t.Errorf("runner volumes %v", runner.Volumes)
	}
	if runner.Environment["RAWBBIT_DBT_ROUTES_FILE"] != "/app/runtime/dbt-routes.json" {
		t.Errorf("routes file %v", runner.Environment["RAWBBIT_DBT_ROUTES_FILE"])
	}
}

// Optional localhost-only rendering of validate-dbt-routes.yml.
func runValidation(t *testing.T, routes, datasets []map[string]any, enabled, expectSuccess bool, expected []map[string]any) {
	t.Helper()
	var defaults map[string]any
	loadYAML(t, role+"/defaults/main.yml", &defaults)
	defaults["rawbbit_two_dbt_routing_enabled"] = enabled
	defaults["rawbbit_two_dbt_routes"] = routes
	defaults["rawbbit_two_datasets"] = datasets
	defaults["rawbbit_two_raw_load_mode"] = "dbt"
	defaults["rawbbit_two_clickhouse_database"] = "custom_default"
	absRole, _ := filepath.Abs(role)
	playTasks := []any{map[string]any{"ansible.builtin.include_tasks": absRole + "/tasks/validate-dbt-routes.yml"}}
	if expected != nil {
		playTasks = append(playTasks, map[string]any{"ansible.builtin.assert": map[string]any{
			"that": []string{"rawbbit_two_resolved_dbt_routes == expected_routes"}}, "vars": map[string]any{"expected_routes": expected}})
	}
	play := []any{map[string]any{"hosts": "localhost", "gather_facts": false, "vars": defaults, "tasks": playTasks}}
	data, err := yaml.Marshal(play)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "validate.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ansible-playbook", "-i", "localhost,", "-c", "local", path).CombinedOutput()
	if (err == nil) != expectSuccess {
		t.Fatalf("success=%v want %v\n%s", err == nil, expectSuccess, out)
	}
}

func requireAnsible(t *testing.T) {
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("Ansible CLI not installed")
	}
}

func TestRenderDisabledMCPMetadataIsResolvedAndDisabledRouteIsOmitted(t *testing.T) {
	requireAnsible(t)
	route := map[string]any{"app_id": "runner_rawbbit", "dataset_id": "runner_rawbbit"}
	datasets := []map[string]any{{"id": "runner_rawbbit", "enabled": false, "database": "runner_data", "table": "events"}}
	disabled := map[string]any{"app_id": "disabled_app", "dataset_id": "runner_rawbbit", "enabled": false}
	runValidation(t, []map[string]any{route, disabled}, datasets, true, true,
		[]map[string]any{{"app_id": "runner_rawbbit", "dataset_id": "runner_rawbbit", "database": "runner_data", "table": "events"}})
}

func TestRenderMissingMetadataDuplicatesAndMalformedDisabledRoutesFail(t *testing.T) {
	requireAnsible(t)
	route := map[string]any{"app_id": "runner_rawbbit", "dataset_id": "runner_rawbbit"}
	datasets := []map[string]any{{"id": "runner_rawbbit", "enabled": false, "database": "runner_data", "table": "events"}}
	with := func(k string, v any) map[string]any {
		c := map[string]any{"app_id": route["app_id"], "dataset_id": route["dataset_id"]}
		c[k] = v
		return c
	}
	unsafe := with("app_id", "../unsafe")
	unsafe["enabled"] = false
	for _, c := range []struct {
		routes, metadata []map[string]any
	}{{[]map[string]any{route}, nil}, {[]map[string]any{route, route}, datasets},
		{[]map[string]any{unsafe}, datasets}, {[]map[string]any{with("enabled", "false")}, datasets}} {
		metadata := c.metadata
		if metadata == nil {
			metadata = []map[string]any{}
		}
		runValidation(t, c.routes, metadata, true, false, nil)
	}
}

func TestRenderEnabledEmptyRouteListIsValid(t *testing.T) {
	requireAnsible(t)
	runValidation(t, []map[string]any{}, []map[string]any{}, true, true, []map[string]any{})
}

func TestRenderDisabledRoutingDoesNotResolveRetainedObsoleteReferences(t *testing.T) {
	requireAnsible(t)
	runValidation(t, []map[string]any{{"app_id": "example_app", "dataset_id": "removed_dataset"}},
		[]map[string]any{}, false, true, []map[string]any{})
}
