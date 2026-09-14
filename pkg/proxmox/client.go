package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/grioghar/lighthouse/pkg/execx"
)

// Guest is one LXC container on the node.
type Guest struct {
	VMID   int
	Name   string
	Status string
	OSType string
	Tags   []string
	// Image is the OCI reference this guest was created from, as recorded by
	// the lighthouse.image tag. Proxmox itself stores no link from a guest to
	// its source image, and the template filename loses the registry and
	// namespace (docker.io/library/alpine:3.20 -> alpine_3.20.tar), so the
	// reference cannot be recovered after the fact -- it has to be recorded.
	Image string
	// Digest is the manifest digest installed when the guest was tagged, so a
	// check does not depend on the template still existing on the node.
	Digest string
}

// OCIManaged reports whether this guest opted into image-digest tracking.
func (g Guest) OCIManaged() bool { return g.Image != "" }

// TagPrefix marks a guest as OCI-managed and carries its source reference.
// Proxmox tags are lowercased and restricted, so the reference is stored
// encoded; see EncodeRef.
const TagPrefix = "lighthouse.image_"

// Client talks to a Proxmox node through a Runner.
type Client struct {
	Run  Runner
	Node string
}

// ListGuests returns the LXC guests on the node.
func (c *Client) ListGuests(ctx context.Context) ([]Guest, error) {
	out, err := execx.Output(ctx, c.Run, "pvesh", "get",
		fmt.Sprintf("/nodes/%s/lxc", c.Node), "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		VMID   any    `json:"vmid"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Tags   string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("proxmox: decoding guest list: %w", err)
	}
	guests := make([]Guest, 0, len(raw))
	for _, r := range raw {
		id, err := toInt(r.VMID)
		if err != nil {
			continue
		}
		g := Guest{VMID: id, Name: r.Name, Status: r.Status}
		for _, t := range strings.Split(r.Tags, ";") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			g.Tags = append(g.Tags, t)
			if ref, ok := DecodeRef(t); ok {
				g.Image = ref
			}
			if d, ok := DecodeDigest(t); ok {
				g.Digest = d
			}
		}
		guests = append(guests, g)
	}
	return guests, nil
}

// Config returns a guest's configuration keys.
func (c *Client) Config(ctx context.Context, vmid int) (map[string]string, error) {
	out, err := execx.Output(ctx, c.Run, "pvesh", "get",
		fmt.Sprintf("/nodes/%s/lxc/%d/config", c.Node, vmid), "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("proxmox: decoding config for %d: %w", vmid, err)
	}
	cfg := make(map[string]string, len(raw))
	for k, v := range raw {
		cfg[k] = fmt.Sprint(v)
	}
	return cfg, nil
}

// ExecInGuest runs a command inside a running LXC via pct exec.
func (c *Client) ExecInGuest(ctx context.Context, vmid int, argv ...string) (string, error) {
	args := append([]string{"exec", strconv.Itoa(vmid), "--"}, argv...)
	return execx.Output(ctx, c.Run, "pct", args...)
}

// PullOCI pulls an image into the node's template store and returns the task's
// UPID. The Proxmox reference pattern requires a tag and rejects @sha256:
// digests, so a pinned digest cannot be requested here.
func (c *Client) PullOCI(ctx context.Context, storage, reference string) (string, error) {
	if strings.Contains(reference, "@") {
		return "", fmt.Errorf("proxmox: oci-registry-pull rejects digest references (%q); use a tag", reference)
	}
	out, err := execx.Output(ctx, c.Run, "pvesh", "create",
		fmt.Sprintf("/nodes/%s/storage/%s/oci-registry-pull", c.Node, storage),
		"--reference", reference)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UPID:") {
			return line, nil
		}
	}
	return "", fmt.Errorf("proxmox: no UPID in oci-registry-pull output: %q", truncate(out, 200))
}

// TaskStatus is a Proxmox task's terminal state.
type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

// WaitTask polls a task to completion. Pulls and guest creation are
// asynchronous: the command returns a UPID immediately and the work continues,
// so callers that skip this see an empty template store and conclude wrongly
// that the pull failed.
func (c *Client) WaitTask(ctx context.Context, upid string, poll time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		out, err := execx.Output(ctx, c.Run, "pvesh", "get",
			fmt.Sprintf("/nodes/%s/tasks/%s/status", c.Node, upid), "--output-format", "json")
		if err != nil {
			return err
		}
		var st TaskStatus
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return fmt.Errorf("proxmox: decoding task status: %w", err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox: task %s failed: %s", upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case string:
		return strconv.Atoi(n)
	case int:
		return n, nil
	}
	return 0, fmt.Errorf("proxmox: cannot read %v as a vmid", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Runner exposes the transport this client uses, so callers can compose
// further hops onto it -- execx.Guest{Base: c.Runner(), VMID: n} reaches
// inside a guest on whatever node this client happens to talk to, without
// needing to know whether that is local or over ssh.
func (c *Client) Runner() Runner { return c.Run }
