package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grioghar/lighthouse/pkg/update"
)

// printAgentReport renders a run for a human.
//
// The ordering is the point. A homelab run produces forty lines of "up to
// date" and three that matter, so results arrive sorted by urgency and the
// things needing a decision -- failures, pending reboots -- are repeated
// underneath where they cannot be scrolled past.
func printAgentReport(rep update.Report, applied bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tTARGET\tRUNTIME\tSTATE\tDETAIL")
	for _, r := range rep.Results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			r.Provider, targetLabel(r), r.Target.Runtime, r.Outcome, oneLine(r.Detail))
	}
	w.Flush()

	fmt.Printf("\n%s in %s\n", rep.Summary(), rep.Duration().Round(time.Millisecond))

	if len(rep.Errors) > 0 {
		fmt.Println("\nProviders that could not enumerate their targets:")
		for _, err := range rep.Errors {
			fmt.Printf("  - %v\n", err)
		}
	}

	// A pending reboot is the one outcome a scheduled run cannot resolve by
	// itself, so it is stated separately rather than left in a DETAIL column
	// forty rows up.
	if pending := rep.RebootPending(); len(pending) > 0 {
		fmt.Println("\nReboot required (not performed):")
		for _, r := range pending {
			fmt.Printf("  - %s\n", targetLabel(r))
		}
		fmt.Println("  On a Proxmox node this takes every guest down; migrate or stop them first.")
	}

	if !applied {
		if n := countOutdated(rep); n > 0 {
			fmt.Printf("\nCheck-only run: nothing was changed. %d target(s) have updates available; "+
				"re-run with --apply to install them.\n", n)
		}
	}
}

func countOutdated(rep update.Report) int {
	n := 0
	for _, r := range rep.Results {
		if r.Outcome == update.Outdated {
			n++
		}
	}
	return n
}

func targetLabel(r update.Result) string {
	if r.Target.Name != "" && r.Target.Name != r.Target.ID {
		return fmt.Sprintf("%s (%s)", r.Target.Name, r.Target.ID)
	}
	return r.Target.ID
}

// oneLine keeps a multi-line error from breaking the table. The full text is
// still available in the structured log.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	const max = 110
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
