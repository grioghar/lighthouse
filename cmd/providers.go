package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/hostenv"
	"github.com/grioghar/lighthouse/pkg/pkgmgr"
)

var providersCommand = NewProvidersCommand()

// NewProvidersCommand reports what this deployment can actually reach.
//
// The most common failure with a modular updater is a provider that silently
// does nothing -- pct missing inside a container, no Docker socket, a node
// flag with no ssh host. This answers that directly instead of leaving it to
// be inferred from an empty run.
func NewProvidersCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "Show what Lighthouse can reach from here, and why",
		Long: `Reports the detected runtime and which update providers are usable.

Run this first when a provider is not doing what you expect: it distinguishes
"not configured" from "configured but unreachable", which look identical in a
run's output.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			env := hostenv.Detect(ctx, execx.Local{})

			fmt.Println("Runtime")
			fmt.Printf("  %s\n\n", env.Explain())

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROVIDER\tUSABLE\tREQUIRES")

			row := func(name string, ok bool, req string) {
				state := "no"
				if ok {
					state = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, state, req)
			}
			row("docker-images", env.HasDocker,
				"a reachable Docker socket")
			row("docker-packages", env.HasDocker,
				"a reachable Docker socket (opt-in via --providers)")
			row("oci-images", env.CanDriveNodeLocally(),
				"pct/pvesh on this machine, or --ssh-host to a node")
			row("lxc-packages", env.CanDriveNodeLocally(),
				"pct/pvesh on this machine, or --ssh-host to a node")
			row("host-packages", true,
				"a supported package manager on the target")
			w.Flush()

			// Knowing which manager will be used locally turns "it reported
			// nothing" into an answerable question.
			fmt.Println()
			if b, err := pkgmgr.Detect(ctx, execx.Local{}); err == nil {
				fmt.Printf("Local package manager: %s\n", b.Name)
			} else {
				fmt.Printf("Local package manager: none detected (%v)\n", err)
			}

			names := make([]string, 0, len(pkgmgr.Backends))
			for _, b := range pkgmgr.Backends {
				names = append(names, b.Name)
			}
			fmt.Printf("Supported package managers: %v\n", names)

			if env.InContainer() && !env.CanDriveNodeLocally() {
				fmt.Println("\nNote: pct and pvesh exist only on a Proxmox node and cannot be " +
					"installed in a container.\nTo manage guests from here, pass " +
					"--ssh-host root@<node> so the commands land on the node itself.")
			}
			return nil
		},
	}
}
