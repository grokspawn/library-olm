package osutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoStagedDir(t *testing.T) {
	testCases := []struct {
		name    string
		destDir string
		setup   func(rootDir string) error
		doFunc  func(tmpDir string) error
		assert  func(t *testing.T, destDir string, err error)
	}{
		{
			name:    "success",
			destDir: "dest",
			doFunc: func(tmpDir string) error {
				return os.WriteFile(filepath.Join(tmpDir, "hello"), []byte("world"), 0600)
			},
			assert: func(t *testing.T, destDir string, err error) {
				require.NoError(t, err)
				require.DirExists(t, destDir)
				entries, err := os.ReadDir(destDir)
				require.NoError(t, err)
				for _, entry := range entries {
					t.Logf("%s %v", entry.Name(), entry.IsDir())
				}
				require.FileExists(t, filepath.Join(destDir, "hello"))
			},
		},
		{
			name:    "failure: cannot create staging directory",
			destDir: "missing/dest",
			doFunc:  func(tmpDir string) error { return nil },
			assert: func(t *testing.T, destDir string, err error) {
				require.ErrorIs(t, err, os.ErrNotExist)
				require.NoDirExists(t, destDir)
			},
		},
		{
			name:    "failure: destination already exists",
			destDir: "dest",
			setup: func(rootDir string) error {
				return os.Mkdir(filepath.Join(rootDir, "dest"), 0755)
			},
			assert: func(t *testing.T, destDir string, err error) {
				require.ErrorIs(t, err, os.ErrExist)
			},
		},
		{
			name:    "failure: populate fails",
			destDir: "dest",
			doFunc: func(tmpDir string) error {
				if err := os.WriteFile(filepath.Join(tmpDir, "hello"), []byte("world"), 0600); err != nil {
					return err
				}
				return errors.New("populate error")
			},
			assert: func(t *testing.T, destDir string, err error) {
				require.ErrorContains(t, err, "populate error")
				require.NoDirExists(t, destDir)
				require.NoFileExists(t, filepath.Join(destDir, "hello"))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			if tc.setup != nil {
				require.NoErrorf(t, tc.setup(rootDir), "setup unexpectedly failed")
			}
			var stageDir string
			destDir := filepath.Join(rootDir, tc.destDir)
			err := DoStagedDir(destDir, func(tmpDir string) error {
				stageDir = tmpDir
				if tc.doFunc != nil {
					return tc.doFunc(tmpDir)
				}
				return nil
			})

			// The staging dir should never exist after StagedUnpack returns.
			require.NoDirExists(t, stageDir)
			tc.assert(t, destDir, err)
		})
	}
}
