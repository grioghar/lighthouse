package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/grioghar/lighthouse/pkg/container"
	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/filters"
	"github.com/grioghar/lighthouse/pkg/hostenv"
	"github.com/grioghar/lighthouse/pkg/providers/dockerimg"
	"github.com/grioghar/lighthouse/pkg/providers/ociguest"
	"github.com/grioghar/lighthouse/pkg/providers/pkgprov"
	"github.com/grioghar/lighthouse/pkg/proxmox"
	t "github.com/grioghar/lighthouse/pkg/types"
	"github.com/grioghar/lighthouse/pkg/update"
)

var agentCommand = NewAgentCommand()

// NewAgentCommand returns `lighthouse agent`, the unified update daemon.
//
// The original Lighthouse loop updates Docker containers and nothing else.
// This one drives every registered provider on one schedule, so a homelab that
// is part Docker, part LXC, part OCI guest and one hypervisor gets a single
// run and a single report instead of four things to correlate by hand.
//
// It is additive: `lighthouse` on its own still runs the Docker-only loop, and
// `lighthouse proxmox` still does a one-shot node check. This is for the case
// where one process should watch everything.
func NewAgentCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run every enabled update provider on one schedule",
		Long: `Checks -- and optionally updates -- everything Lighthouse can reach.

Providers are selected automatically from what is configured and reachable:

  docker-images    containers whose image has a newer version
  oci-images       Proxmox guests created from an OCI image (needs --node)
  lxc-packages     packages inside Proxmox LXC guests (needs --node)
  host-packages    packages on the host or Proxmox node itself
  docker-packages  packages inside running containers (opt-in; see --providers)

Nothing is changed unless --apply is passed. A check never installs, removes,
restarts or recreates anything, so running this on a schedule against a
hypervisor is safe by construction.

The node itself is never rebooted. A pending reboot is reported and left for a
human to schedule, because on a hypervisor that means every guest going down.`,
		RunE:         runAgent,
		SilenceUsage: true,
	}
	f := cmd.Flags()
	f.String("schedule", "", "cron expression; empty runs once and exits")
	f.Duration("interval", 0, "run every interval (alternative to --schedule)")
	f.Bool("apply", false, "actually install updates; without it, nothing is changed")
	f.StringSlice("providers", nil, "limit to these providers (default: all auto-enabled)")
	f.StringSlice("include", nil, "only act on targets matching these names or globs")
	f.StringSlice("exclude", nil, "skip targets matching these names or globs")
	f.Int("concurrency", update.DefaultConcurrency, "targets checked at once per provider")

	f.String("node", "", "Proxmox node name; enables the Proxmox providers")
	f.String("ssh-host", "", "reach the node over ssh (e.g. root@10.0.0.1)")
	f.String("ssh-key", "", "ssh key file; empty uses your ssh config and agent")
	f.String("storage", "local", "storage holding OCI templates")
	f.String("template-dir", "/var/lib/vz/template/cache", "where oci-registry-pull writes")
	f.String("registry-auth", "", "base64 registry credentials for private images")
	f.Duration("timeout", execx.DefaultTimeout, "per-command timeout")

	f.Bool("host-packages", false, "keep the host itself updated even without --node")
	f.String("package-manager", "", "force a package manager instead of detecting one")
	f.Bool("include-stopped-guests", false, "also try stopped LXC guests (they cannot be entered)")
	f.Bool("pull-oci", false, "pull newer OCI images for outdated guests (does not rebuild)")
	return cmd
}

func runAgent(cmd *cobra.Command, names []string) error {
	f := cmd.Flags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := hostenv.Detect(ctx, execx.Local{})
	log.Info("lighthouse agent: " + env.Explain())

	reg, err := buildRegistry(ctx, cmd, names, env)
	if err != nil {
		return err
	}
	if len(reg.Names()) == 0 {
		return fmt.Errorf("no providers are enabled; pass --node for Proxmox, " +
			"--host-packages for this machine, or run where a Docker socket is reachable")
	}

	selected, _ := f.GetStringSlice("providers")
	providers, err := reg.Select(selected)
	if err != nil {
		return err
	}

	apply, _ := f.GetBool("apply")
	include, _ := f.GetStringSlice("include")
	exclude, _ := f.GetStringSlice("exclude")
	concurrency, _ := f.GetInt("concurrency")

	engine := &update.Engine{
		Providers:   providers,
		Apply:       apply,
		Concurrency: concurrency,
		Include:     include,
		Exclude:     exclude,
	}

	names2 := make([]string, 0, len(providers))
	for _, p := range providers {
		names2 = append(names2, p.Name())
	}
	mode := "check-only (pass --apply to install)"
	if apply {
		mode = "applying updates"
	}
	log.Infof("providers: %s; %s", strings.Join(names2, ", "), mode)

	schedule, _ := f.GetString("schedule")
	interval, _ := f.GetDuration("interval")
	if schedule == "" && interval == 0 {
		return runOnceAndReport(ctx, engine)
	}
	return runScheduled(ctx, engine, schedule, interval)
}

// buildRegistry enables the providers this deployment can actually use.
//
// Auto-enabling rather than requiring a flag per provider is deliberate: the
// common deployments -- a container on the node, a container elsewhere pointed
// at it over ssh, a binary on the node itself -- each imply a different set,
// and making the operator work that out is how a provider ends up silently not
// running.
func buildRegistry(ctx context.Context, cmd *cobra.Command, names []string, env hostenv.Env) (*update.Registry, error) {
	f := cmd.Flags()
	reg := update.NewRegistry()

	node, _ := f.GetString("node")
	sshHost, _ := f.GetString("ssh-host")
	sshKey, _ := f.GetString("ssh-key")
	timeout, _ := f.GetDuration("timeout")
	manager, _ := f.GetString("package-manager")
	opts := pkgprov.Options{Manager: manager}

	// Docker: enabled whenever a daemon is reachable, which is the normal case
	// for a container with the socket bind-mounted.
	//
	// The flags are read here rather than taken from the package globals
	// because those are populated by the root command's PreRun, which cobra
	// does not run for subcommands. Reading them from the inherited flag set
	// is what keeps `lighthouse agent` honouring every Docker flag that
	// `lighthouse` itself does.
	if env.HasDocker {
		reg.Add(newDockerProvider(cmd, names, timeout))
	}

	// Proxmox: everything here needs pct and pvesh, which exist only on the
	// node. Refusing early with the reason beats a run where every guest fails
	// with "pct: command not found".
	if node != "" {
		if sshHost == "" && !env.CanDriveNodeLocally() {
			return nil, fmt.Errorf(
				"--node was given but pct/pvesh are not reachable from here (%s). "+
					"They exist only on the Proxmox node and cannot be installed in a "+
					"container -- pass --ssh-host root@<node> so the commands land there",
				env.Kind)
		}
		runner := proxmox.NewRunner(sshHost, sshKey, timeout)
		client := &proxmox.Client{Run: runner, Node: node}

		storage, _ := f.GetString("storage")
		tmplDir, _ := f.GetString("template-dir")
		regAuth, _ := f.GetString("registry-auth")
		pull, _ := f.GetBool("pull-oci")
		goos, goarch := nodePlatform(ctx, client)
		reg.Add(&ociguest.Provider{
			Client:      client,
			Resolver:    registryResolver{auth: regAuth, goos: goos, goarch: goarch},
			TemplateDir: tmplDir,
			Storage:     storage,
			Pull:        pull,
		})

		includeStoppedGuests, _ := f.GetBool("include-stopped-guests")
		guests, err := pkgprov.NewGuests(client, opts)
		if err != nil {
			return nil, err
		}
		guests.IncludeStopped = includeStoppedGuests
		reg.Add(guests)

		// The node's own packages, over the same transport as everything else.
		host, err := pkgprov.NewHost(runner, node, opts)
		if err != nil {
			return nil, err
		}
		reg.Add(host)
		return reg, nil
	}

	if hostPkgs, _ := f.GetBool("host-packages"); hostPkgs {
		runner := proxmox.NewRunner(sshHost, sshKey, timeout)
		label := env.NodeName
		if label == "" {
			label = "localhost"
		}
		host, err := pkgprov.NewHost(runner, env.NodeName, opts)
		if err != nil {
			return nil, err
		}
		host.Label = label
		reg.Add(host)
	}
	return reg, nil
}

// newDockerProvider builds the Docker provider from the root command's
// persistent flags, which subcommands inherit.
func newDockerProvider(cmd *cobra.Command, names []string, timeout time.Duration) *dockerimg.Provider {
	f := cmd.Flags()
	b := func(name string) bool { v, _ := f.GetBool(name); return v }
	s := func(name string) string { v, _ := f.GetString(name); return v }

	disabled, _ := f.GetStringSlice("disable-containers")
	filter, _ := filters.BuildFilter(names, disabled, b("label-enable"), s("scope"))

	client := container.NewClient(container.ClientOptions{
		IncludeStopped:    b("include-stopped"),
		ReviveStopped:     b("revive-stopped"),
		RemoveVolumes:     b("remove-volumes"),
		IncludeRestarting: b("include-restarting"),
		WarnOnHeadFailed:  container.WarningStrategy(s("warn-on-head-failure")),
	})
	return &dockerimg.Provider{
		Client: client,
		Host:   dockerHost(),
		Params: t.UpdateParams{
			Filter:         filter,
			Cleanup:        b("cleanup"),
			NoRestart:      b("no-restart"),
			Timeout:        timeout,
			LifecycleHooks: b("enable-lifecycle-hooks"),
			RollingRestart: b("rolling-restart"),
			NoPull:         b("no-pull"),
		},
	}
}

func dockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "local docker"
}

func runOnceAndReport(ctx context.Context, e *update.Engine) error {
	rep := e.Run(ctx)
	printAgentReport(rep, e.Apply)
	// A target that failed its check is a real failure and should be visible
	// to whatever ran this -- a systemd timer, a CI job, a cron mail.
	for _, r := range rep.Results {
		if r.Outcome == update.Failed {
			return fmt.Errorf("%d target(s) failed", len(rep.Actionable()))
		}
	}
	if len(rep.Errors) > 0 {
		return rep.Errors[0]
	}
	return nil
}

func runScheduled(ctx context.Context, e *update.Engine, schedule string, interval time.Duration) error {
	if schedule == "" {
		// robfig/cron's @every takes a duration string directly, so an
		// interval is just a schedule spelled differently.
		schedule = "@every " + interval.String()
	}
	c := cron.New()
	if err := c.AddFunc(schedule, func() {
		rep := e.Run(ctx)
		printAgentReport(rep, e.Apply)
		for _, err := range rep.Errors {
			log.WithError(err).Error("provider error")
		}
	}); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	c.Start()
	log.Infof("scheduled: %s", schedule)

	<-ctx.Done()
	c.Stop()
	log.Info("shutting down")
	return nil
}
