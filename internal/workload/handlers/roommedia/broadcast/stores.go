package broadcast

import (
	"os"
	"path/filepath"
	"strings"
)

const PackageStoreDir = "package"

func PackageStoreRoot(cacheDir string) string { return filepath.Join(cacheDir, PackageStoreDir) }

func RemovePackageStores(cacheDir string) error {
	if strings.TrimSpace(cacheDir) == "" {
		return nil
	}
	return os.RemoveAll(PackageStoreRoot(cacheDir))
}
