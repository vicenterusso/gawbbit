// Command rawbbit-provision-datasets safely provisions optional dataset
// databases and read-only MCP identities. It only accepts protected file
// paths, runs as root on the analytics VM and sends ClickHouse admin
// credentials only to the loopback HTTP endpoint.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/cli"
	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/provision"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr, provision.Options{RequireRoot: true}))
}

func run(prog string, argv []string, stdout, stderr io.Writer, opts provision.Options) int {
	args, err := cli.Parse(prog, argv, stdout, stderr)
	if err != nil {
		var exit *cli.ExitError
		if errors.As(err, &exit) {
			return exit.Code
		}
		return 2
	}
	opts.PreflightOnly = args.PreflightOnly
	changed, err := provision.Run(args.Config, args.RuntimeConfig, args.Schema, args.Journal, opts)
	if err != nil {
		var perr *provision.Error
		if errors.As(err, &perr) {
			fmt.Fprintf(stderr, "dataset provisioner: %s\n", perr.Msg)
		} else {
			fmt.Fprintf(stderr, "dataset provisioner: unexpected failure: %s\n", err)
		}
		return 1
	}
	switch {
	case args.PreflightOnly:
		fmt.Fprintln(stdout, "dataset provisioner: preflight passed")
	case changed:
		fmt.Fprintln(stdout, "dataset provisioner: configuration reconciled")
	default:
		fmt.Fprintln(stdout, "dataset provisioner: configuration unchanged")
	}
	return 0
}
