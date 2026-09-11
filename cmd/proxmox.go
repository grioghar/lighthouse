package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	ref "github.com/distribution/reference"
	"github.com/spf13/cobra"

	"github.com/grioghar/lighthouse/pkg/proxmox"
	"github.com/grioghar/lighthouse/pkg/registry/auth"
	"github.com/grioghar/lighthouse/pkg/registry/digest"
)

var proxmoxCommand = NewProxmoxCommand()

// NewProxmoxCommand returns the `lighthouse proxmox` subcommand, which keeps
// Proxmox VE guests current.
//
// It is a subcommand rather than part of the main loop because it manages a
// different thing by a different mechanism: the Docker path talks to a daemon
// socket, this one drives pvesh/pct on a node, and the two share only the
// registry digest lookup.
func NewProxmoxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxmox",
		Short: "Check, and optionally update, Proxmox VE guests",
		Long: `Keeps Proxmox VE guests up to date.

Three independent modes, selected with --mode:

  oci        Compare the installed OCI manifest digest of guests tagged with a
             lighthouse image reference against the registry. Read-only.
  recreate   Also pull a newer image for out-of-date guests. Implies oci.
             Destructive: Proxmox squashes image layers into one rootfs when
             the guest is created, so there is no in-place swap -- updating
             means rebuilding the guest.
  packages   Run the distribution package manager inside conventional guests.
             This is unrelated to image digests and is the only thing that
             updates a plain Debian or Ubuntu LXC.

Guests opt into OCI tracking by carrying a tag written by --tag-guest, because
Proxmox records no link between a guest and the image it came from, and the
template filename drops the registry and namespace.`,
		RunE: runProxmox,
		// A node that says "CT is locked" is a runtime condition, not a usage
		// mistake; printing the full flag list on top of it buries the reason.
		SilenceUsage: true,
	}
	f := cmd.Flags()
	f.String("node", "", "Proxmox node name (required)")
	f.String("mode", "oci", "comma-separated: oci, recreate, packages, or off")
	f.String("storage", "local", "storage holding OCI templates")
	f.String("template-dir", "/var/lib/vz/template/cache",
		"where oci-registry-pull writes (the template cache, not the import dir)")
	f.String("ssh-host", "", "run node commands over ssh (e.g. root@10.0.0.1); empty runs them locally")
	f.String("ssh-key", "", "ssh key file; empty uses your ssh config and agent")
	f.Bool("dry-run", true, "report what would change without changing it")
	f.String("registry-auth", "", "base64 registry credentials for private images")
	f.Duration("timeout", proxmox.DefaultTimeout, "per-command timeout")
	f.Int("tag-guest", 0, "tag this VMID as OCI-managed and exit (use with --image)")
	f.String("image", "", "OCI reference to record, with --tag-guest")
	return cmd
}

func runProxmox(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	node, _ := f.GetString("node")
	if node == "" {
		return fmt.Errorf("--node is required")
	}
	modeStr, _ := f.GetString("mode")
	modes, err := proxmox.ParseModes(modeStr)
	if err != nil {
		return err
	}
	sshHost, _ := f.GetString("ssh-host")
	sshKey, _ := f.GetString("ssh-key")
	timeout, _ := f.GetDuration("timeout")

	var runner proxmox.Runner = proxmox.ExecRunner{Timeout: timeout}
	if sshHost != "" {
		runner = proxmox.SSHRunner{Host: sshHost, KeyFile: sshKey, Timeout: timeout}
	}
	client := &proxmox.Client{Run: runner, Node: node}
	ctx := context.Background()

	if vmid, _ := f.GetInt("tag-guest"); vmid != 0 {
		image, _ := f.GetString("image")
		if image == "" {
			return fmt.Errorf("--tag-guest needs --image")
		}
		return tagGuest(ctx, client, vmid, image)
	}
	if len(modes) == 0 {
		return fmt.Errorf("--mode selects nothing; pass oci, recreate and/or packages")
	}

	regAuth, _ := f.GetString("registry-auth")
	storage, _ := f.GetString("storage")
	tmplDir, _ := f.GetString("template-dir")
	dryRun, _ := f.GetBool("dry-run")

	u := &proxmox.Updater{
		Client:      client,
		Modes:       modes,
		Resolver:    registryResolver{auth: regAuth},
		TemplateDir: tmplDir,
		Storage:     storage,
		DryRun:      dryRun,
	}
	results, err := u.Check(ctx)
	if err != nil {
		return err
	}
	return report(results, dryRun)
}

func tagGuest(ctx context.Context, c *proxmox.Client, vmid int, image string) error {
	cfg, err := c.Config(ctx, vmid)
	if err != nil {
		return err
	}
	tags := cfg["tags"]
	// Drop any previous reference tag so re-tagging replaces rather than stacks.
	var kept []string
	for _, t := range splitTags(tags) {
		if _, ours := proxmox.DecodeRef(t); !ours {
			kept = append(kept, t)
		}
	}
	kept = append(kept, proxmox.EncodeRef(image), proxmox.HumanTag(image))
	if _, err := c.Run.Run(ctx, "pct", "set", fmt.Sprint(vmid), "--tags", joinTags(kept)); err != nil {
		return err
	}
	fmt.Printf("CT %d now tracks %s\n", vmid, image)
	return nil
}

func splitTags(s string) []string {
	var out []string
	for _, t := range splitAny(s, ";,") {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitAny(s, seps string) []string {
	out := []string{""}
	for _, r := range s {
		if containsRune(seps, r) {
			out = append(out, "")
			continue
		}
		out[len(out)-1] += string(r)
	}
	return out
}

func containsRune(s string, r rune) bool {
	for _, x := range s {
		if x == r {
			return true
		}
	}
	return false
}

func joinTags(t []string) string {
	s := ""
	for i, x := range t {
		if i > 0 {
			s += ";"
		}
		s += x
	}
	return s
}

func report(results []proxmox.Result, dryRun bool) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VMID\tNAME\tKIND\tSTATE\tDETAIL")
	var outdated, failed int
	for _, r := range results {
		state := "ok"
		if r.Outdated {
			state = "outdated"
			outdated++
		}
		detail := r.Action
		if r.Err != nil {
			state, detail = "error", r.Err.Error()
			failed++
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Guest.VMID, r.Guest.Name, r.Kind, state, detail)
	}
	w.Flush()
	fmt.Printf("\n%d guest(s) checked, %d outdated, %d error(s)\n", len(results), outdated, failed)
	if dryRun && outdated > 0 {
		fmt.Println("dry run: nothing was changed. Re-run with --dry-run=false to apply.")
	}
	return nil
}

// registryResolver resolves a bare image reference to its current manifest
// digest, reusing the same auth and digest code the Docker path uses.
type registryResolver struct{ auth string }

func (r registryResolver) Digest(_ context.Context, reference string) (string, error) {
	named, err := ref.ParseNormalizedNamed(reference)
	if err != nil {
		return "", fmt.Errorf("parsing %q: %w", reference, err)
	}
	named = ref.TagNameOnly(named)
	tag := "latest"
	if t, ok := named.(ref.Tagged); ok {
		tag = t.Tag()
	}

	challengeURL := auth.GetChallengeURL(named)
	req, err := auth.GetChallengeRequest(challengeURL)
	if err != nil {
		return "", err
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	token, err := auth.GetBearerHeader(res.Header.Get("WWW-Authenticate"), named, r.auth)
	if err != nil {
		return "", err
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s",
		challengeURL.Scheme, challengeURL.Host, ref.Path(named), tag)
	return digest.GetDigest(manifestURL, token)
}
