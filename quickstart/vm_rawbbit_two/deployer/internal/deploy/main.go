package deploy

import (
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/deployer/internal/pyjson"
)

// Main runs the CLI and returns the exit code.
func Main(argv []string, stdout, stderr io.Writer) int {
	args, err := ParseArgs(argv)
	if errors.Is(err, ErrHelp) {
		fmt.Fprint(stdout, Help)
		return 0
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		fmt.Fprintf(stderr, "%srawbbit-deploy-dbt-runner: error: %s\n", Usage, usage.Msg)
		return 2
	}
	if err := run(args, stdout); err != nil {
		var known *Error
		if errors.As(err, &known) {
			fmt.Fprintln(stderr, known.Msg)
		} else {
			// Do not stringify arbitrary parser/OS errors: they may include config.
			fmt.Fprintln(stderr, errHost.Error())
		}
		return 1
	}
	return 0
}

func run(args *Args, stdout io.Writer) error {
	if !args.drainPositive {
		return fail("Drain timeout must be positive")
	}
	if !args.ValidateOnly && !args.InspectMCPEnv && runtime.GOOS != "linux" {
		return fail("Cutover requires the Linux Docker host and a shared bind-mount flock")
	}
	d, err := New(args, Run)
	if err != nil {
		return err
	}
	d.Stdout = stdout
	switch {
	case args.InspectMCPEnv:
		changed, err := d.MCPEnvironmentChanged()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, pyjson.Encode(pyjson.NewObject("changed", changed), pyjson.EncodeOptions{}))
	case args.CheckQuiescence:
		if err := d.CheckQuiescence(); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Server quiescence verified; caller must retain the shared pipeline lock")
	case !args.ValidateOnly:
		return d.Execute()
	}
	return nil
}
