package proxmox

import (
	"archive/tar"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// Proxmox guest tags are lowercased and accept a restricted character set, so
// a reference like docker.io/library/alpine:3.20 cannot be stored verbatim.
// Hex-encoding is reversible, survives the lowercasing, and keeps the tag
// within the permitted alphabet -- at the cost of being unreadable in the UI,
// which is why HumanTag is written alongside it.
const (
	refTagPrefix   = "lighthouse-image-"
	humanTagPrefix = "lighthouse-src-"
)

// EncodeRef renders an OCI reference as a Proxmox-safe guest tag.
func EncodeRef(reference string) string {
	return refTagPrefix + hex.EncodeToString([]byte(reference))
}

// DecodeRef recovers a reference from a guest tag, reporting whether the tag
// was one of ours.
func DecodeRef(tag string) (string, bool) {
	if !strings.HasPrefix(tag, refTagPrefix) {
		return "", false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(tag, refTagPrefix))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

var unsafeTagChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// HumanTag is a lossy, readable companion tag so the reference is visible in
// the Proxmox UI. It is never parsed back -- DecodeRef is the source of truth.
func HumanTag(reference string) string {
	return humanTagPrefix + unsafeTagChars.ReplaceAllString(strings.ToLower(reference), "-")
}

// TemplateFile is the filename oci-registry-pull writes for a reference.
// Proxmox derives it from the last path element and the tag, discarding the
// registry and namespace entirely.
func TemplateFile(reference string) string {
	ref := reference
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	return fmt.Sprintf("%s_%s.tar", path.Base(name), tag)
}

// ociIndex is the subset of an OCI image index we need.
type ociIndex struct {
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

// InstalledDigest reads the manifest digest out of a pulled template.
//
// A template produced by oci-registry-pull is a full OCI layout -- oci-layout,
// index.json and blobs/sha256/... -- so what is installed can be determined
// locally, with no registry round trip. The index carries no annotations,
// though, so the source reference is NOT recoverable from it.
func InstalledDigest(r io.Reader) (string, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("proxmox: no index.json in template (not an OCI layout?)")
		}
		if err != nil {
			return "", fmt.Errorf("proxmox: reading template: %w", err)
		}
		if path.Clean(hdr.Name) != "index.json" {
			continue
		}
		var idx ociIndex
		if err := json.NewDecoder(tr).Decode(&idx); err != nil {
			return "", fmt.Errorf("proxmox: decoding index.json: %w", err)
		}
		if len(idx.Manifests) == 0 {
			return "", fmt.Errorf("proxmox: index.json lists no manifests")
		}
		return idx.Manifests[0].Digest, nil
	}
}

// InstalledDigestOnNode reads the digest of a template stored on the node.
func (c *Client) InstalledDigestOnNode(ctx context.Context, templatePath string) (string, error) {
	// Stream just index.json rather than the whole archive: a squashed rootfs
	// template can be gigabytes, and only the index is needed.
	out, err := c.Run.Run(ctx, "tar", "xOf", templatePath, "index.json")
	if err != nil {
		return "", err
	}
	var idx ociIndex
	if err := json.Unmarshal([]byte(out), &idx); err != nil {
		return "", fmt.Errorf("proxmox: decoding index.json from %s: %w", templatePath, err)
	}
	if len(idx.Manifests) == 0 {
		return "", fmt.Errorf("proxmox: %s lists no manifests", templatePath)
	}
	return idx.Manifests[0].Digest, nil
}
