package internal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	npminstall "github.com/paketo-buildpacks/npm-install"
)

func Run(executablePath, appDir string, symlinkResolver npminstall.SymlinkResolver) error {
	fname := strings.Split(executablePath, "/")
	layerPath := filepath.Join(fname[:len(fname)-2]...)
	if filepath.IsAbs(executablePath) {
		layerPath = fmt.Sprintf("/%s", layerPath)
	}

	linkPath, err := os.Readlink(filepath.Join(appDir, "node_modules"))
	if err != nil {
		return err
	}

	linkPath, err = filepath.Abs(linkPath)
	if err != nil {
		return err
	}

	fileInfo, err := os.Stat(linkPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if fileInfo != nil && fileInfo.IsDir() {
		return nil
	}

	err = resolveWorkspaceModules(symlinkResolver, appDir, layerPath)
	if err != nil {
		return err
	}

	err = createSymlink(filepath.Join(layerPath, "node_modules"), linkPath)
	if err != nil {
		return err
	}

	cacheFolder := filepath.Join(os.TempDir(), npminstall.NODE_MODULES_CACHE)
	return os.Mkdir(cacheFolder, os.ModePerm)
}

func resolveWorkspaceModules(symlinkResolver npminstall.SymlinkResolver, appDir, layerPath string) error {
	lockFile, err := symlinkResolver.ParseLockfile(filepath.Join(appDir, "package-lock.json"))
	if err != nil {
		return err
	}

	for _, pkg := range lockFile.Packages {
		if pkg.Link {
			linkPath, err := os.Readlink(filepath.Join(appDir, pkg.Resolved))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				} else {
					return err
				}
			}

			err = createSymlink(filepath.Join(layerPath, pkg.Resolved), linkPath)
			if err != nil {
				return err
			}
		}
	}

	return nil

}

func createSymlink(target, source string) error {
	err := os.RemoveAll(source)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(source), os.ModePerm)
	if err != nil {
		return err
	}

	return os.Symlink(target, source)
}
