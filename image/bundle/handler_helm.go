package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/downloader"

	"github.com/operator-framework/library-olm/image"
	"github.com/operator-framework/library-olm/image/internal/ociutil"
	"github.com/operator-framework/library-olm/image/internal/osutil"
)

// Helm OCI artifact media types as defined by the Helm specification.
// See https://helm.sh/docs/topics/registries/
const (
	// HelmConfigMediaType is the OCI config media type for Helm chart artifacts.
	HelmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"

	// HelmChartContentMediaType is the layer media type containing the chart archive.
	HelmChartContentMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	// HelmProvenanceMediaType is the layer media type containing the provenance file.
	HelmProvenanceMediaType = "application/vnd.cncf.helm.chart.provenance.v1.prov"
)

// HelmChartHandler is an [image.Handler] that unpacks Helm chart OCI artifacts.
// It matches OCI manifests whose config descriptor has media type
// [HelmConfigMediaType], then writes the chart package and optional provenance
// file into the destination directory. It does not parse or validate the chart
// archive; callers can do that after unpacking.
//
// This produces the inverse of what `helm push` stores:
//   - <name>-<version>.tgz      — the chart package (always written)
//   - <name>-<version>.tgz.prov — the provenance file (when verification is enabled)
//
// Provenance handling is controlled by [HelmChartHandler.Verify]:
//   - [downloader.VerifyNever]:      skip provenance entirely
//   - [downloader.VerifyIfPossible]: write and verify provenance if present, succeed silently if absent
//   - [downloader.VerifyAlways]:     write and verify provenance, error if absent or invalid
//   - [downloader.VerifyLater]:      write provenance (error if absent), skip verification
type HelmChartHandler struct {
	// Verify controls provenance verification behavior. The default
	// (VerifyNever) skips verification entirely.
	Verify downloader.VerificationStrategy

	// Keyring is the path to the PGP keyring file used to verify chart
	// provenance. Required when Verify is VerifyAlways, or when Verify is
	// VerifyIfPossible and a provenance layer is present.
	Keyring string
}

func (h *HelmChartHandler) Name() string { return "helm.sh/chart" }

func (h *HelmChartHandler) Matches(_ context.Context, _ image.Repository, desc ocispecv1.Descriptor, manifestBytes []byte) (bool, error) {
	if !image.IsManifest(desc.MediaType) {
		return false, nil
	}

	var m ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return false, fmt.Errorf("parsing manifest: %w", err)
	}

	return m.Config.MediaType == HelmConfigMediaType, nil
}

func (h *HelmChartHandler) Discover(ctx context.Context, repo image.Repository, desc ocispecv1.Descriptor, manifestBytes []byte) ([]ocispecv1.Descriptor, error) {
	return ociutil.DiscoverManifestDescriptors(ctx, repo, desc, manifestBytes)
}

// Unpack stages the selected layers beside dest and publishes them together by
// renaming the staging directory. The parent of dest must exist. Replacing an
// existing empty dest directory depends on the platform's rename behavior;
// a nonempty dest directory is never replaced.
func (h *HelmChartHandler) Unpack(ctx context.Context, repo image.Repository, _ ocispecv1.Descriptor, manifestBytes []byte, dest string) error {
	plan, err := h.planUnpack(ctx, repo, manifestBytes)
	if err != nil {
		return err
	}
	return osutil.DoStagedDir(dest, func(stagedDir string) error {
		return h.doUnpack(ctx, repo, plan, stagedDir)
	})
}

type helmUnpackPlan struct {
	basename         string
	chartLayer       ocispecv1.Descriptor
	provenanceLayer  *ocispecv1.Descriptor
	verifyProvenance bool
}

func (h *HelmChartHandler) planUnpack(ctx context.Context, repo image.Repository, manifestBytes []byte) (helmUnpackPlan, error) {
	var m ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return helmUnpackPlan{}, fmt.Errorf("parsing manifest: %w", err)
	}

	cfg, err := h.fetchHelmConfig(ctx, repo, m.Config)
	if err != nil {
		return helmUnpackPlan{}, err
	}

	chartLayer := ociutil.FindFirstLayer(m.Layers, HelmChartContentMediaType)
	if chartLayer == nil {
		return helmUnpackPlan{}, fmt.Errorf("layer with media type %s not found", HelmChartContentMediaType)
	}

	plan := helmUnpackPlan{
		basename:   fmt.Sprintf("%s-%s", cfg.Name, cfg.Version),
		chartLayer: *chartLayer,
	}
	provLayer := ociutil.FindFirstLayer(m.Layers, HelmProvenanceMediaType)
	switch {
	case h.Verify == downloader.VerifyNever:
		// No provenance handling.
	case provLayer == nil && h.Verify == downloader.VerifyIfPossible:
		// No provenance available, not required.
	case provLayer == nil:
		// VerifyAlways or VerifyLater both require the provenance layer.
		return helmUnpackPlan{}, fmt.Errorf("provenance data required but no provenance layer found")
	default:
		plan.provenanceLayer = provLayer
		plan.verifyProvenance = h.Verify != downloader.VerifyLater
	}
	return plan, nil
}

func (h *HelmChartHandler) fetchHelmConfig(ctx context.Context, repo image.Repository, desc ocispecv1.Descriptor) (*chart.Metadata, error) {
	reader, err := repo.FetchBlob(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetching helm config: %w", err)
	}

	configBlob, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("reading helm config: %w", err), reader.Close())
	}

	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("closing helm config: %w", err)
	}

	var cfg chart.Metadata
	if err := json.Unmarshal(configBlob, &cfg); err != nil {
		return nil, fmt.Errorf("reading helm config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating helm config: %w", err)
	}
	return &cfg, nil
}

func (h *HelmChartHandler) doUnpack(ctx context.Context, repo image.Repository, plan helmUnpackPlan, dest string) error {
	chartPath := filepath.Join(dest, plan.basename+".tgz")
	if err := ociutil.WriteBlobFile(ctx, repo, plan.chartLayer, chartPath); err != nil {
		return fmt.Errorf("writing chart package: %w", err)
	}
	if plan.provenanceLayer == nil {
		return nil
	}
	provPath := chartPath + ".prov"
	if err := ociutil.WriteBlobFile(ctx, repo, *plan.provenanceLayer, provPath); err != nil {
		return fmt.Errorf("writing provenance file: %w", err)
	}
	if !plan.verifyProvenance {
		return nil
	}
	if _, err := downloader.VerifyChart(chartPath, provPath, h.Keyring); err != nil {
		return fmt.Errorf("verifying chart provenance: %w", err)
	}
	return nil
}
