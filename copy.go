package partitionresizer

import (
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/filesystem"
)

func CopyFileSystem(src fs.FS, dst filesystem.FileSystem) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			if path == "." {
				return nil
			}
			return dst.Mkdir(path)
		}

		if !info.Mode().IsRegular() {
			// FAT32 / ISO / SquashFS should not have others
			return nil
		}

		return copyOneFile(src, dst, path, info)
	})
}

func copyOneFile(src fs.FS, dst filesystem.FileSystem, path string, info fs.FileInfo) error {
	in, err := src.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := dst.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Restore timestamps *after* data is written (tar semantics)
	return dst.Chtimes(
		path,
		info.ModTime(), // creation time fallback if not available
		time.Now(),     // access time: optional / policy choice
		info.ModTime(),
	)
}
