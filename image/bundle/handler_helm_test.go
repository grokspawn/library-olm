package bundle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/manifest"
	"helm.sh/helm/v4/pkg/downloader"

	"github.com/operator-framework/library-olm/image/internal/testutil"
)

// buildHelmManifest creates a Helm OCI artifact manifest with the given config,
// chart content blob, and optional extra layers. Returns the manifest descriptor and bytes.
func buildHelmManifest(repo *testutil.FakeRepo, cfg string, chartContent []byte, extraLayers ...ocispecv1.Descriptor) (ocispecv1.Descriptor, []byte) {
	configDesc := repo.AddBlob([]byte(cfg), HelmConfigMediaType)
	chartLayerDesc := repo.AddBlob(chartContent, HelmChartContentMediaType)

	layers := append([]ocispecv1.Descriptor{chartLayerDesc}, extraLayers...)
	manifestBytes := testutil.BuildManifest(configDesc, layers...)
	desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)
	return desc, manifestBytes
}

// buildSignedHelmManifest creates a Helm OCI artifact using the real signtest
// fixtures from Helm's test data. Returns the manifest descriptor and bytes.
func buildSignedHelmManifest(t *testing.T, repo *testutil.FakeRepo) (ocispecv1.Descriptor, []byte) {
	t.Helper()
	chartData, err := os.ReadFile("testdata/signtest-0.1.0.tgz")
	require.NoError(t, err)
	provData, err := os.ReadFile("testdata/signtest-0.1.0.tgz.prov")
	require.NoError(t, err)

	provDesc := repo.AddBlob(provData, HelmProvenanceMediaType)
	return buildHelmManifest(repo,
		`{"apiVersion":"v2","name":"signtest","version":"0.1.0"}`,
		chartData,
		provDesc,
	)
}

type trackedHelmReader struct {
	io.Reader
	closeErr error
	closed   bool
}

func (r *trackedHelmReader) Close() error {
	r.closed = true
	return r.closeErr
}

func assertNoHelmOutput(t *testing.T, root, dest string) {
	t.Helper()
	_, err := os.Stat(dest)
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "staging directory left behind")
}

// --- Tests ---

func TestHelmChartHandler_Name(t *testing.T) {
	h := &HelmChartHandler{}
	assert.Equal(t, "helm.sh/chart", h.Name())
}

func TestHelmChartHandler_Matches(t *testing.T) {
	ctx := context.Background()

	t.Run("HelmConfigMediaType", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)
		manifestBytes := testutil.BuildManifest(configDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		matched, err := h.Matches(ctx, repo, desc, manifestBytes)
		require.NoError(t, err)
		assert.True(t, matched)
	})

	t.Run("DockerManifest/HelmConfig", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)
		manifestBytes := testutil.BuildManifest(configDesc)
		desc := repo.AddManifest(manifestBytes, manifest.DockerV2Schema2MediaType)

		h := &HelmChartHandler{}
		matched, err := h.Matches(ctx, repo, desc, manifestBytes)
		require.NoError(t, err)
		assert.True(t, matched)
	})

	t.Run("NonHelmConfigMediaType", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := testutil.SetupSingleManifest(repo, map[string]string{
			"some-label": "value",
		}, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		matched, err := h.Matches(ctx, repo, desc, manifestBytes)
		require.NoError(t, err)
		assert.False(t, matched)
	})

	t.Run("UnrecognizedManifestMediaType", func(t *testing.T) {
		h := &HelmChartHandler{}
		desc := ocispecv1.Descriptor{MediaType: "application/vnd.unknown"}
		matched, err := h.Matches(ctx, testutil.NewFakeRepo(), desc, []byte("{}"))
		require.NoError(t, err)
		assert.False(t, matched)
	})

	t.Run("MalformedManifest", func(t *testing.T) {
		h := &HelmChartHandler{}
		desc := ocispecv1.Descriptor{MediaType: ocispecv1.MediaTypeImageManifest}
		matched, err := h.Matches(ctx, testutil.NewFakeRepo(), desc, []byte("not-json"))
		require.Error(t, err)
		assert.False(t, matched)
		assert.Contains(t, err.Error(), "parsing manifest")
	})
}

func TestHelmChartHandler_Discover(t *testing.T) {
	ctx := context.Background()

	t.Run("ReturnsAllDescriptors", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)
		chartLayerDesc := repo.AddBlob([]byte("chart-data"), HelmChartContentMediaType)

		manifestBytes := testutil.BuildManifest(configDesc, chartLayerDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		descs, err := h.Discover(ctx, repo, desc, manifestBytes)
		require.NoError(t, err)

		require.Len(t, descs, 3)
		assert.Equal(t, desc.Digest, descs[0].Digest)
		assert.Equal(t, configDesc.Digest, descs[1].Digest)
		assert.Equal(t, chartLayerDesc.Digest, descs[2].Digest)
	})

	t.Run("IncludesProvenanceLayer", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)
		chartLayerDesc := repo.AddBlob([]byte("chart-data"), HelmChartContentMediaType)
		provDesc := repo.AddBlob([]byte("prov-data"), HelmProvenanceMediaType)

		manifestBytes := testutil.BuildManifest(configDesc, chartLayerDesc, provDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		descs, err := h.Discover(ctx, repo, desc, manifestBytes)
		require.NoError(t, err)

		require.Len(t, descs, 4)
	})

	t.Run("InvalidManifest", func(t *testing.T) {
		h := &HelmChartHandler{}
		desc := ocispecv1.Descriptor{MediaType: ocispecv1.MediaTypeImageManifest}
		_, err := h.Discover(ctx, testutil.NewFakeRepo(), desc, []byte("not-json"))
		require.Error(t, err)
	})
}

func TestHelmChartHandler_Unpack(t *testing.T) {
	ctx := context.Background()

	t.Run("WritesChartPackage", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("fake-tgz-content"),
		)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "fake-tgz-content")
	})

	t.Run("RejectsPathTraversalMetadata", func(t *testing.T) {
		tests := []struct {
			name         string
			config       string
			outsideChart string
		}{
			{
				name:         "parent path in name",
				config:       `{"apiVersion":"v2","name":"../outside","version":"0.1.0"}`,
				outsideChart: "outside-0.1.0.tgz",
			},
			{
				name:   "current path in name",
				config: `{"apiVersion":"v2","name":"./inside","version":"0.1.0"}`,
			},
			{
				name:   "parent path in version",
				config: `{"apiVersion":"v2","name":"mychart","version":"../../../outside"}`,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				repo := testutil.NewFakeRepo()
				desc, manifestBytes := buildHelmManifest(repo, tt.config, []byte("chart-data"))
				root := t.TempDir()
				dest := filepath.Join(root, "dest")
				require.NoError(t, os.Mkdir(dest, 0755))

				h := &HelmChartHandler{}
				err := h.Unpack(ctx, repo, desc, manifestBytes, dest)
				require.ErrorContains(t, err, "validating helm config")

				entries, err := os.ReadDir(dest)
				require.NoError(t, err)
				assert.Empty(t, entries)
				if tt.outsideChart != "" {
					testutil.AssertFileNotExists(t, filepath.Join(root, tt.outsideChart))
				}
			})
		}
	})

	t.Run("VerifyNever/SkipsProvenance", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		provDesc := repo.AddBlob([]byte("provenance-data"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
			provDesc,
		)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "chart-data")
		testutil.AssertFileNotExists(t, filepath.Join(dest, "mychart-0.1.0.tgz.prov"))
	})

	t.Run("VerifyNever/NoProvenance", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileNotExists(t, filepath.Join(dest, "mychart-0.1.0.tgz.prov"))
	})

	t.Run("VerifyAlways/ValidSignature", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildSignedHelmManifest(t, repo)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{
			Verify:  downloader.VerifyAlways,
			Keyring: "testdata/helm-test-key.pub",
		}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileExists(t, filepath.Join(dest, "signtest-0.1.0.tgz"))
		testutil.AssertFileExists(t, filepath.Join(dest, "signtest-0.1.0.tgz.prov"))
	})

	t.Run("VerifyAlways/NoProvenance", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)

		h := &HelmChartHandler{
			Verify:  downloader.VerifyAlways,
			Keyring: "testdata/helm-test-key.pub",
		}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "provenance data required")
	})

	t.Run("VerifyAlways/InvalidSignature", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		chartData, err := os.ReadFile("testdata/signtest-0.1.0.tgz")
		require.NoError(t, err)

		badProvDesc := repo.AddBlob([]byte("not-a-valid-signature"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"signtest","version":"0.1.0"}`,
			chartData,
			badProvDesc,
		)

		h := &HelmChartHandler{
			Verify:  downloader.VerifyAlways,
			Keyring: "testdata/helm-test-key.pub",
		}
		err = h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying chart provenance")
	})

	t.Run("VerifyIfPossible/ValidSignature", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildSignedHelmManifest(t, repo)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{
			Verify:  downloader.VerifyIfPossible,
			Keyring: "testdata/helm-test-key.pub",
		}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileExists(t, filepath.Join(dest, "signtest-0.1.0.tgz"))
		testutil.AssertFileExists(t, filepath.Join(dest, "signtest-0.1.0.tgz.prov"))
	})

	t.Run("VerifyIfPossible/NoProvenance", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{
			Verify:  downloader.VerifyIfPossible,
			Keyring: "testdata/helm-test-key.pub",
		}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "chart-data")
		testutil.AssertFileNotExists(t, filepath.Join(dest, "mychart-0.1.0.tgz.prov"))
	})

	t.Run("VerifyIfPossible/InvalidSignature", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		chartData, err := os.ReadFile("testdata/signtest-0.1.0.tgz")
		require.NoError(t, err)

		badProvDesc := repo.AddBlob([]byte("not-a-valid-signature"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"signtest","version":"0.1.0"}`,
			chartData,
			badProvDesc,
		)

		h := &HelmChartHandler{
			Verify:  downloader.VerifyIfPossible,
			Keyring: "testdata/helm-test-key.pub",
		}
		err = h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying chart provenance")
	})

	t.Run("VerifyLater/WritesProvNoVerification", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		// Use invalid provenance data — VerifyLater should not attempt verification
		badProvDesc := repo.AddBlob([]byte("unverified-prov"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
			badProvDesc,
		)

		dest := filepath.Join(t.TempDir(), "dest")
		h := &HelmChartHandler{Verify: downloader.VerifyLater}
		require.NoError(t, h.Unpack(ctx, repo, desc, manifestBytes, dest))

		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "chart-data")
		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz.prov"), "unverified-prov")
	})

	t.Run("VerifyLater/NoProvenance", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)

		h := &HelmChartHandler{Verify: downloader.VerifyLater}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "provenance data required")
	})

	t.Run("NoChartContentLayer", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)
		provLayer := repo.AddBlob([]byte("provenance"), HelmProvenanceMediaType)

		manifestBytes := testutil.BuildManifest(configDesc, provLayer)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), HelmChartContentMediaType)
	})

	t.Run("ChartLayerFetchFails", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)

		missingLayerDesc := ocispecv1.Descriptor{
			MediaType: HelmChartContentMediaType,
			Digest:    digest.FromString("missing-layer"),
			Size:      1,
		}

		manifestBytes := testutil.BuildManifest(configDesc, missingLayerDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "writing chart package")
	})

	t.Run("ProvenanceFetchFails", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		chartLayerDesc := repo.AddBlob([]byte("chart-data"), HelmChartContentMediaType)
		configDesc := repo.AddBlob([]byte(`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`), HelmConfigMediaType)

		missingProvDesc := ocispecv1.Descriptor{
			MediaType: HelmProvenanceMediaType,
			Digest:    digest.FromString("missing-prov"),
			Size:      1,
		}

		manifestBytes := testutil.BuildManifest(configDesc, chartLayerDesc, missingProvDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{Verify: downloader.VerifyAlways, Keyring: "testdata/helm-test-key.pub"}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "writing provenance file")
	})

	t.Run("ConfigFetchFails", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		missingConfigDesc := ocispecv1.Descriptor{
			MediaType: HelmConfigMediaType,
			Digest:    digest.FromString("missing-config"),
			Size:      1,
		}

		chartLayerDesc := repo.AddBlob([]byte("chart-data"), HelmChartContentMediaType)

		manifestBytes := testutil.BuildManifest(missingConfigDesc, chartLayerDesc)
		desc := repo.AddManifest(manifestBytes, ocispecv1.MediaTypeImageManifest)

		h := &HelmChartHandler{}
		err := h.Unpack(ctx, repo, desc, manifestBytes, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fetching helm config")
	})

	t.Run("MalformedManifest", func(t *testing.T) {
		h := &HelmChartHandler{}
		desc := ocispecv1.Descriptor{MediaType: ocispecv1.MediaTypeImageManifest}
		err := h.Unpack(ctx, testutil.NewFakeRepo(), desc, []byte("not-json"), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing manifest")
	})
}

func TestHelmChartHandler_UnpackStagesBeforePublishing(t *testing.T) {
	ctx := context.Background()

	t.Run("PublishesUnvalidatedChartBytes", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("not-a-chart-archive"),
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		require.NoError(t, (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest))
		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "not-a-chart-archive")
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "dest", entries[0].Name())
	})

	t.Run("DestinationWithTrailingSeparator", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		require.NoError(t, (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest+string(os.PathSeparator)))
		testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "chart-data")
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "dest", entries[0].Name())
	})

	t.Run("MissingProvenanceDoesNotStageChart", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		err := (&HelmChartHandler{Verify: downloader.VerifyAlways}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorContains(t, err, "provenance data required")
		assert.EqualValues(t, 1, repo.FetchBlobCount.Load(), "only the config blob should be fetched")
		assertNoHelmOutput(t, root, dest)
	})

	t.Run("InvalidProvenanceRemovesStagedFiles", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		chartData, err := os.ReadFile("testdata/signtest-0.1.0.tgz")
		require.NoError(t, err)
		prov := repo.AddBlob([]byte("invalid-signature"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"signtest","version":"0.1.0"}`,
			chartData, prov,
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		err = (&HelmChartHandler{Verify: downloader.VerifyAlways, Keyring: "testdata/helm-test-key.pub"}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorContains(t, err, "verifying chart provenance")
		assertNoHelmOutput(t, root, dest)
	})

	t.Run("ChartReadOrCloseErrorRemovesStagedFiles", func(t *testing.T) {
		readErr := errors.New("chart read failed")
		closeErr := errors.New("chart close failed")
		for _, tt := range []struct {
			name     string
			reader   io.Reader
			closeErr error
			wantErr  error
		}{
			{"read", iotest.ErrReader(readErr), nil, readErr},
			{"close", strings.NewReader("chart-data"), closeErr, closeErr},
		} {
			t.Run(tt.name, func(t *testing.T) {
				repo := testutil.NewFakeRepo()
				desc, manifestBytes := buildHelmManifest(repo,
					`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
					[]byte("chart-data"),
				)
				blob := &trackedHelmReader{Reader: tt.reader, closeErr: tt.closeErr}
				repo.Blobs[digest.FromString("chart-data").String()] = func() io.ReadCloser { return blob }
				root := t.TempDir()
				dest := filepath.Join(root, "dest")
				err := (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest)
				require.ErrorIs(t, err, tt.wantErr)
				assert.True(t, blob.closed)
				assertNoHelmOutput(t, root, dest)
			})
		}
	})

	t.Run("ChartFetchErrorRemovesStagingDirectory", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)
		delete(repo.Blobs, digest.FromString("chart-data").String())
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		err := (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorContains(t, err, "writing chart package")
		assertNoHelmOutput(t, root, dest)
	})

	t.Run("ProvenanceFetchErrorRemovesStagedChart", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		prov := repo.AddBlob([]byte("provenance-data"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"), prov,
		)
		delete(repo.Blobs, prov.Digest.String())
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		err := (&HelmChartHandler{Verify: downloader.VerifyLater}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorContains(t, err, "writing provenance file")
		assertNoHelmOutput(t, root, dest)
	})

	t.Run("ProvenanceCloseErrorRemovesStagedFiles", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		prov := repo.AddBlob([]byte("provenance-data"), HelmProvenanceMediaType)
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"), prov,
		)
		closeErr := errors.New("provenance close failed")
		blob := &trackedHelmReader{Reader: strings.NewReader("provenance-data"), closeErr: closeErr}
		repo.Blobs[prov.Digest.String()] = func() io.ReadCloser { return blob }
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		err := (&HelmChartHandler{Verify: downloader.VerifyLater}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorIs(t, err, closeErr)
		assert.True(t, blob.closed)
		assertNoHelmOutput(t, root, dest)
	})

	t.Run("NonemptyDestinationIsNotReplaced", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		require.NoError(t, os.Mkdir(dest, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dest, "existing"), []byte("keep"), 0o600))
		err := (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest)
		require.ErrorContains(t, err, "renaming staged directory")
		testutil.AssertFileContent(t, filepath.Join(dest, "existing"), "keep")
		testutil.AssertFileNotExists(t, filepath.Join(dest, "mychart-0.1.0.tgz"))
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "dest", entries[0].Name())
	})

	t.Run("ExistingEmptyDestinationIsCleanOnError", func(t *testing.T) {
		repo := testutil.NewFakeRepo()
		desc, manifestBytes := buildHelmManifest(repo,
			`{"apiVersion":"v2","name":"mychart","version":"0.1.0"}`,
			[]byte("chart-data"),
		)
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		require.NoError(t, os.Mkdir(dest, 0o700))
		err := (&HelmChartHandler{}).Unpack(ctx, repo, desc, manifestBytes, dest)
		if err == nil {
			testutil.AssertFileContent(t, filepath.Join(dest, "mychart-0.1.0.tgz"), "chart-data")
		} else {
			require.ErrorContains(t, err, "renaming staged directory")
			entries, readErr := os.ReadDir(dest)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		}
		entries, readErr := os.ReadDir(root)
		require.NoError(t, readErr)
		require.Len(t, entries, 1)
		assert.Equal(t, "dest", entries[0].Name())
	})
}
