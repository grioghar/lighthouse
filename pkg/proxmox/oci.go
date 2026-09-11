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
	refTagPrefix    = "lighthouse-image-"
	humanTagPrefix  = "lighthouse-src-"
	digestTagPrefix = "lighthouse-digest-"
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

// manifestIndex is a multi-arch image index as returned by a registry.
type manifestIndex struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// PlatformDigest picks the digest to compare an installed image against.
//
// This is the difference between a working check and one that always reports
// drift. A registry HEAD on a multi-arch tag returns the digest of the *index*,
// while Proxmox stores the digest of the single platform manifest it actually
// pulled. Those are never equal, so comparing them marks every multi-arch
// guest permanently outdated -- and under recreate, rebuilds it forever.
//
// Given an index, return the sub-manifest matching os/arch. Given a plain
// manifest, fall back to the digest the registry reported for the tag.
func PlatformDigest(body []byte, headerDigest, goos, goarch string) (string, error) {
	var idx manifestIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return "", fmt.Errorf("proxmox: decoding manifest: %w", err)
	}
	if len(idx.Manifests) == 0 {
		// A single-platform manifest: the tag's own digest is the right thing.
		if headerDigest == "" {
			return "", fmt.Errorf("proxmox: registry returned no digest for a single-platform manifest")
		}
		return headerDigest, nil
	}
	for _, m := range idx.Manifests {
		// Skip attestation/provenance entries, which carry unknown/unknown.
		if m.Platform.OS == goos && m.Platform.Architecture == goarch {
			return m.Digest, nil
		}
	}
	return "", fmt.Errorf("proxmox: image index has no %s/%s manifest", goos, goarch)
}

// EncodeDigest records an installed manifest digest as a guest tag. The hex
// body of a sha256 digest is already a legal tag, so only the algorithm
// prefix needs removing.
func EncodeDigest(digest string) string {
	return digestTagPrefix + strings.TrimPrefix(digest, "sha256:")
}

// DecodeDigest recovers a recorded digest from a guest tag.
//
// Recording the digest on the guest matters because the alternative -- reading
// it back out of the template -- assumes the template is still on the node and
// still named after the current tag. Templates are large and routinely pruned,
// and retagging a guest changes the derived filename, so that assumption
// breaks in normal use.
func DecodeDigest(tag string) (string, bool) {
	if !strings.HasPrefix(tag, digestTagPrefix) {
		return "", false
	}
	hexBody := strings.TrimPrefix(tag, digestTagPrefix)
	if len(hexBody) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(hexBody); err != nil {
		return "", false
	}
	return "sha256:" + hexBody, true
}
