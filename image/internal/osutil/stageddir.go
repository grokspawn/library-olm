package osutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DoStagedDir populates a staged temporary directory beside dest and publishes
// it by renaming the directory to dest. The parent of dest must already exist.
// The callback must close all files it opens before returning. If the callback
// or rename fails, DoStagedDir removes the staging directory and joins any
// cleanup error with the original error.
func DoStagedDir(dest string, do func(string) error) error {
	dest = filepath.Clean(dest)
	stagedDir, err := os.MkdirTemp(filepath.Dir(dest), ".image-stage-")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}

	if err := do(stagedDir); err != nil {
		return errors.Join(err, removeStagedDir(stagedDir))
	}
	if err := os.Rename(stagedDir, dest); err != nil {
		return errors.Join(fmt.Errorf("renaming staged directory: %w", err), removeStagedDir(stagedDir))
	}
	return nil
}

func removeStagedDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing staging directory: %w", err)
	}
	return nil
}
