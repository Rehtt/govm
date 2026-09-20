package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// extractArchive extracts a Go archive into dest and returns the directory
// containing the archive's Go tree (normally dest/go). Archive entries are
// validated before they can create files, and links are rejected entirely.
func extractArchive(archivePath, dest string) (string, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		if err := extractZip(archivePath, dest); err != nil {
			return "", err
		}
	} else {
		if err := extractTarGz(archivePath, dest); err != nil {
			return "", err
		}
	}
	root, err := findGoRoot(dest)
	if err != nil {
		return "", err
	}
	return root, nil
}

// ExtractArchive is the exported form used by integrations and tests.
func ExtractArchive(archivePath, dest string) (string, error) {
	return extractArchive(archivePath, dest)
}

func extractTarGz(archivePath, dest string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open gzip archive: %w", err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	seen := make(map[string]struct{})
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar archive: %w", err)
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("archive contains duplicate entry %q", header.Name)
		}
		seen[name] = struct{}{}
		target, err := archiveTarget(dest, name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := makeArchiveDir(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := writeArchiveFile(target, tarReader, header.Size, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			// Metadata records are interpreted by archive/tar and are not
			// filesystem entries.
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive contains an unsafe link %q", header.Name)
		default:
			return fmt.Errorf("archive contains unsupported entry %q", header.Name)
		}
	}
	return nil
}

func extractZip(archivePath, dest string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	defer reader.Close()
	seen := make(map[string]struct{})
	for _, entry := range reader.File {
		name, err := safeArchiveName(entry.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("archive contains duplicate entry %q", entry.Name)
		}
		seen[name] = struct{}{}
		target, err := archiveTarget(dest, name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("archive contains an unsafe link %q", entry.Name)
		}
		if entry.FileInfo().IsDir() || strings.HasSuffix(entry.Name, "/") {
			if err := makeArchiveDir(target, mode); err != nil {
				return err
			}
			continue
		}
		file, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open archive entry %q: %w", entry.Name, err)
		}
		writeErr := writeArchiveFile(target, file, int64(entry.UncompressedSize64), mode)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return fmt.Errorf("close archive entry %q: %w", entry.Name, closeErr)
		}
	}
	return nil
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return "", fmt.Errorf("archive contains an invalid path %q", name)
	}
	// Treat backslashes as separators even on Unix so an archive safe on one
	// host cannot become a traversal on Windows.
	name = strings.ReplaceAll(name, `\`, "/")
	drivePath := len(name) >= 2 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':'
	if strings.HasPrefix(name, "/") || drivePath || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive contains an absolute path %q", name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == ".." {
			return "", fmt.Errorf("archive path traversal is not allowed: %q", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive contains an unsafe path %q", name)
	}
	return filepath.FromSlash(clean), nil
}

func archiveTarget(dest, name string) (string, error) {
	target := filepath.Join(dest, name)
	if !samePathOrChild(target, dest) {
		return "", fmt.Errorf("archive path escapes destination: %q", name)
	}
	if err := ensureNoSymlinkComponents(dest, filepath.Dir(target)); err != nil {
		return "", err
	}
	return target, nil
}

func ensureNoSymlinkComponents(root, targetDir string) error {
	rel, err := filepath.Rel(root, targetDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive path escapes destination")
	}
	current := root
	if info, statErr := os.Lstat(current); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive destination is a symbolic link")
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive path traverses a symbolic link: %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("archive path parent is not a directory: %q", current)
		}
	}
	return nil
}

func makeArchiveDir(target string, mode os.FileMode) error {
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("archive entry conflicts with existing path %q", target)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create archive directory %q: %w", target, err)
	}
	if mode.Perm() != 0 {
		_ = os.Chmod(target, mode.Perm())
	}
	return nil
}

func writeArchiveFile(target string, reader io.Reader, size int64, mode os.FileMode) error {
	if size < 0 {
		return fmt.Errorf("archive entry %q has a negative size", target)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("archive entry conflicts with existing path %q", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, normalizedArchiveMode(mode))
	if err != nil {
		return fmt.Errorf("create archive file %q: %w", target, err)
	}
	_, copyErr := io.CopyN(file, reader, size)
	if copyErr == nil {
		var extra [1]byte
		if n, readErr := reader.Read(extra[:]); readErr == nil && n > 0 {
			copyErr = fmt.Errorf("archive entry is larger than declared")
		}
	}
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("extract archive file %q: %w", target, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
	}
	if mode.Perm() != 0 {
		_ = os.Chmod(target, mode.Perm())
	}
	return nil
}

func normalizedArchiveMode(mode os.FileMode) os.FileMode {
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	return perm
}

func findGoRoot(dest string) (string, error) {
	if validGoTree(dest) {
		return dest, nil
	}
	direct := filepath.Join(dest, "go")
	if validGoTree(direct) {
		return direct, nil
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(dest, entry.Name())
		if validGoTree(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("archive does not contain a valid Go tree (missing bin/go or src/make.bash)")
}

func validGoTree(root string) bool {
	versionInfo, err := os.Stat(filepath.Join(root, "VERSION"))
	if err != nil || !versionInfo.Mode().IsRegular() {
		return false
	}
	if hasGoExecutable(root) {
		return true
	}
	_, scriptErr := os.Stat(filepath.Join(root, "src", "make.bash"))
	if runtime.GOOS == "windows" {
		_, scriptErr = os.Stat(filepath.Join(root, "src", "make.bat"))
	}
	return scriptErr == nil
}

func validBinaryTree(root string) bool {
	versionInfo, err := os.Stat(filepath.Join(root, "VERSION"))
	return err == nil && versionInfo.Mode().IsRegular() && hasGoExecutable(root)
}
