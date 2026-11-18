package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type cliCase struct {
	Name   string            `json:"name"`
	Argv   []string          `json:"argv"`
	Docker map[string]string `json:"docker"`
	Files  map[string]string `json:"files"`
	HTTP   *string           `json:"http"`
	Expect struct {
		Exit        int                          `json:"exit"`
		Stdout      string                       `json:"stdout"`
		Stderr      string                       `json:"stderr"`
		State       map[string]map[string]string `json:"state"`
		DockerCalls []string                     `json:"docker_calls"`
		UpCalled    bool                         `json:"up_called"`
	} `json:"expect"`
}

var verifiedAt = regexp.MustCompile(`"verified_at": [0-9.e+]+`)

// TestCLIMatchesPythonGoldens replays the deployer CLI against a fake docker
// executable and compares exit code, stdout, stderr, published files and the
// exact docker command sequence with outputs recorded from the original
// deploy-dbt-runner.py on identical fixtures.
func TestCLIMatchesPythonGoldens(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX sh required for the fake docker CLI")
	}
	data, err := os.ReadFile("../../testdata/cli_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []cliCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs("../../testdata/fake-docker.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) { replayCLI(t, c, fake) })
	}
}

func replayCLI(t *testing.T, c cliCase, fake string) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ctx := map[string]string{"root": root, "install": root + "/install", "stage": root + "/stage", "runtime": root + "/runtime"}
	for _, d := range []string{"install", "stage", "runtime", "docker", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(ctx["runtime"], root+"/runtime-link"); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if c.HTTP == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(*c.HTTP))
	})}
	go server.Serve(listener) //nolint:errcheck // closed below
	defer server.Close()
	ctx["port"] = fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	sub := func(s string) string {
		for k, v := range ctx {
			s = strings.ReplaceAll(s, "{"+k+"}", v)
		}
		return s
	}
	for rel, content := range c.Files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(sub(content)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range c.Docker {
		if err := os.WriteFile(filepath.Join(root, "docker", name), []byte(sub(content)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script, _ := os.ReadFile(fake)
	if err := os.WriteFile(filepath.Join(root, "bin", "docker"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+"/bin:/usr/bin:/bin")
	t.Setenv("FAKE_DOCKER_DIR", root+"/docker")
	t.Setenv("FAKE_RUNTIME", ctx["runtime"])
	argv := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		argv[i] = sub(a)
	}
	var stdout, stderr bytes.Buffer
	code := Main(argv, &stdout, &stderr)
	if code != c.Expect.Exit || stdout.String() != c.Expect.Stdout || stderr.String() != c.Expect.Stderr {
		t.Fatalf("exit %d stdout %q stderr %q\nwant exit %d stdout %q stderr %q",
			code, stdout.String(), stderr.String(), c.Expect.Exit, c.Expect.Stdout, c.Expect.Stderr)
	}
	state := map[string]map[string]string{}
	for _, top := range []string{"install", "runtime"} {
		_ = filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			content, _ := os.ReadFile(path)
			text := string(content)
			if info.Name() == "deploy-fence.json" {
				text = verifiedAt.ReplaceAllString(text, `"verified_at": <t>`)
			}
			state[rel] = map[string]string{"mode": fmt.Sprintf("0o%o", info.Mode().Perm()),
				"data": strings.ReplaceAll(text, root, "{root}")}
			return nil
		})
	}
	if !reflect.DeepEqual(state, c.Expect.State) && !(len(state) == 0 && len(c.Expect.State) == 0) {
		keys := func(m map[string]map[string]string) []string {
			var out []string
			for k := range m {
				out = append(out, k)
			}
			sort.Strings(out)
			return out
		}
		for _, k := range keys(c.Expect.State) {
			if !reflect.DeepEqual(state[k], c.Expect.State[k]) {
				t.Errorf("%s: %v, want %v", k, state[k], c.Expect.State[k])
			}
		}
		t.Fatalf("state keys %v, want %v", keys(state), keys(c.Expect.State))
	}
	calls, _ := os.ReadFile(root + "/docker/calls.log")
	got := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(calls), root, "{root}"), "\n"), "\n")
	if len(calls) == 0 {
		got = []string{}
	}
	want := c.Expect.DockerCalls
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docker calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(root + "/docker/up.called"); (err == nil) != c.Expect.UpCalled {
		t.Fatalf("up called mismatch")
	}
}
