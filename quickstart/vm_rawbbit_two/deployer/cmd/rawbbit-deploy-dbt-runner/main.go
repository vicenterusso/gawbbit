// Command rawbbit-deploy-dbt-runner is the fail-closed VM-two host cutover
// for the dbt-runner service (formerly deploy-dbt-runner.py). Exit codes:
// 0 success, 1 deployment/host failure, 2 invalid arguments.
package main

import (
	"os"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/deployer/internal/deploy"
)

func main() {
	os.Exit(deploy.Main(os.Args[1:], os.Stdout, os.Stderr))
}
