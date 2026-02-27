package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// cloudImage is one entry from the Talos cloud-images.json release asset.
type cloudImage struct {
	Cloud   string `json:"cloud"`
	Version string `json:"version"`
	Region  string `json:"region"`
	Arch    string `json:"arch"`
	Type    string `json:"type"`
	ID      string `json:"id"`
}

// cloudImagesURL returns the URL to the cloud-images.json file published with
// each Talos release on GitHub.
func cloudImagesURL(talosVersion string) string {
	return fmt.Sprintf(
		"https://github.com/siderolabs/talos/releases/download/%s/cloud-images.json",
		talosVersion,
	)
}

// FindTalosAMI returns the official Talos AMI ID for the given version, region,
// and architecture by fetching the cloud-images.json published with each release.
//
// talosVersion must be in the form "v1.9.0".
// arch must be "amd64" or "arm64" (default "amd64").
func FindTalosAMI(ctx context.Context, region, talosVersion, arch string) (string, error) {
	if arch == "" {
		arch = "amd64"
	}

	url := cloudImagesURL(talosVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for cloud-images.json: %w", err)
	}
	// Follow redirects (GitHub releases redirect to release-assets CDN).
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch cloud-images.json for Talos %s: %w", talosVersion, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"cloud-images.json returned HTTP %d for Talos %s — version may not exist",
			resp.StatusCode, talosVersion,
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read cloud-images.json: %w", err)
	}

	var images []cloudImage
	if err := json.Unmarshal(body, &images); err != nil {
		return "", fmt.Errorf("parse cloud-images.json: %w", err)
	}

	for _, img := range images {
		if img.Cloud == "aws" && img.Region == region && img.Arch == arch {
			return img.ID, nil
		}
	}

	return "", fmt.Errorf(
		"no official Talos %s AMI found for region %s arch %s — "+
			"check https://github.com/siderolabs/talos/releases or use --ami-id to provide your own",
		talosVersion, region, arch,
	)
}
