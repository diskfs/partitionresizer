package partitionresizer

import (
	"io"
	"io/fs"
	"os"

	"github.com/diskfs/go-diskfs/filesystem"
)

var excludedPaths = map[string]bool{
	"lost+found":                true,
	".DS_Store":                 true,
	"System Volume Information": true,
}

func CopyFileSystem(src fs.FS, dst filesystem.FileSystem) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// filter out special directories/files
		if excludedPaths[d.Name()] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == "." || path == "/" || path == "\\" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		// symlinks, when they exist
		if info.Mode()&os.ModeSymlink != 0 {
			// Check if your destination interface supports symlinks
			// Most custom 'filesystem.FileSystem' interfaces might not.
			return handleSymlink(src, dst, path)
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

	out, err := dst.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Restore timestamps *after* data is written (tar semantics)
	atime := getAccessTime(info)
	return dst.Chtimes(
		path,
		info.ModTime(), // creation time fallback if not available
		atime,          // access time: optional / policy choice
		info.ModTime(),
	)
}

func handleSymlink(src fs.FS, dst filesystem.FileSystem, path string) error {
	// Note: src must support ReadLink. If src is an os.DirFS,
	// you might need a type assertion or use os.Readlink directly.
	linkTarget, err := os.Readlink(path)
	if err != nil {
		return nil // Or handle error
	}

	// This assumes your 'dst' interface has a Symlink method
	return dst.Symlink(linkTarget, path)
}
