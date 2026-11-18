package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argparse_golden.json was recorded from the original provision_datasets.py
// (CPython argparse) for every line of argcases.txt at three terminal widths.
// exit -1 means the arguments parsed successfully.
func TestParseMatchesPythonArgparse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "argparse_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Args    string  `json:"args"`
		Columns *string `json:"columns"`
		Exit    int     `json:"exit"`
		Stdout  string  `json:"stdout"`
		Stderr  string  `json:"stderr"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Columns != nil {
			t.Setenv("COLUMNS", *c.Columns)
		} else {
			t.Setenv("COLUMNS", "")
		}
		var stdout, stderr bytes.Buffer
		args, err := Parse("provision_datasets.py", strings.Fields(c.Args), &stdout, &stderr)
		var exit *ExitError
		switch {
		case c.Exit == -1:
			if err != nil || args == nil {
				t.Errorf("%q: expected successful parse, got %v (%s)", c.Args, err, stderr.String())
			}
		case !errors.As(err, &exit) || exit.Code != c.Exit:
			t.Errorf("%q: exit %v, want %d", c.Args, err, c.Exit)
		case stdout.String() != c.Stdout || stderr.String() != c.Stderr:
			t.Errorf("%q (COLUMNS=%v):\nstdout %q\n  want %q\nstderr %q\n  want %q", c.Args, c.Columns, stdout.String(), c.Stdout, stderr.String(), c.Stderr)
		}
	}
}

func TestParseValues(t *testing.T) {
	args, err := Parse("p", strings.Fields("--conf=a --runtime b --sch c --j d --pre"), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if *args != (Args{Config: "a", RuntimeConfig: "b", Schema: "c", Journal: "d", PreflightOnly: true}) {
		t.Fatalf("args %+v", args)
	}
}
