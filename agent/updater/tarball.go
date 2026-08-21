// tarball.go — bundle tar.gz 解包（仅接受扁平文件名，防路径穿越）。
package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func extractTarGz(bundle []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("bundle: non-regular entry %q", hdr.Name)
		}
		base := filepath.Base(hdr.Name)
		if base != hdr.Name || base == "." || base == ".." {
			return fmt.Errorf("bundle: non-flat entry %q", hdr.Name)
		}
		if hdr.Size > 64<<20 {
			return fmt.Errorf("bundle: entry too large %q", hdr.Name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, base), b, 0o755); err != nil {
			return err
		}
	}
}
