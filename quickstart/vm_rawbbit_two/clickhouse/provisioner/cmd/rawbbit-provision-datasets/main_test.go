package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/provision"
)

func TestRunRequiresRootAndReportsSafely(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	var stdout, stderr bytes.Buffer
	code := run("rawbbit-provision-datasets", strings.Fields("--config a --runtime-config b --schema c --journal d"),
		&stdout, &stderr, provision.Options{RequireRoot: true})
	if code != 1 || stdout.Len() != 0 || stderr.String() != "dataset provisioner: dataset provisioning must run as root\n" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunUsageErrorsExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("rawbbit-provision-datasets", nil, &stdout, &stderr, provision.Options{}); code != 2 ||
		!strings.Contains(stderr.String(), "the following arguments are required: --config, --runtime-config, --schema, --journal") {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
	stdout.Reset()
	if code := run("rawbbit-provision-datasets", []string{"--help"}, &stdout, &stderr, provision.Options{}); code != 0 ||
		!strings.HasPrefix(stdout.String(), "usage: rawbbit-provision-datasets [-h]") {
		t.Fatalf("help code %d: %q", code, stdout.String())
	}
}

func TestRunReportsProvisionErrorsOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run("p", strings.Fields("--config /nonexistent/config.json --runtime-config b --schema c --journal d --preflight-only"),
		&stdout, &stderr, provision.Options{RequireRoot: false})
	if code != 1 || stderr.String() != "dataset provisioner: protected JSON file is missing or invalid\n" {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
}
