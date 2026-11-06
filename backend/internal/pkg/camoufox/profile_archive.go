package camoufox

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	MaxProfileArchiveSize  = 256 << 20
	MaxProfileFileSize     = 256 << 20
	MaxProfileTotalSize    = 1 << 30
	MaxProfileFiles        = 20000
	MaxArchiveManifestSize = 8 << 20
)

// ValidateProfileArchive runs before extraction or account creation. Names must
// be safe on both Unix and Windows, so a backup remains portable between hosts.
func ValidateProfileArchive(archive *zip.Reader) error {
	if len(archive.File) > MaxProfileFiles {
		return errors.New("account archive has too many files")
	}
	seen := make(map[string]bool)
	regularFiles := make(map[string]bool)
	var total uint64
	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "" || strings.ContainsAny(name, "\\:\x00") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
			return errors.New("account archive contains an unsafe path")
		}
		for _, part := range strings.Split(name, "/") {
			for _, char := range part {
				if char < 32 {
					return errors.New("account archive contains a control character in a path")
				}
			}
			base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
			if part == "." || part == ".." || strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, "<>\"|?*") || base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9') {
				return errors.New("account archive contains an unsafe path component")
			}
		}
		key := strings.ToLower(name)
		if seen[key] {
			return errors.New("account archive contains duplicate paths")
		}
		seen[key] = true
		if file.Mode()&os.ModeSymlink != 0 || (!file.Mode().IsRegular() && !file.FileInfo().IsDir()) {
			return errors.New("account archive contains a link or special file")
		}
		if !file.FileInfo().IsDir() {
			regularFiles[key] = true
		}
		parts := strings.Split(name, "/")
		if name != "accounts.json" {
			if len(parts) < 2 || parts[0] != "profiles" {
				return errors.New("account archive contains an unknown entry")
			}
			id, err := uuid.Parse(parts[1])
			if err != nil || id.String() != parts[1] || (len(parts) == 2 && !file.FileInfo().IsDir()) {
				return errors.New("account archive contains an invalid profile ID")
			}
		} else if file.FileInfo().IsDir() || file.UncompressedSize64 > MaxArchiveManifestSize {
			return errors.New("account archive manifest is invalid or too large")
		}
		if file.UncompressedSize64 > MaxProfileFileSize {
			return errors.New("account archive contains an oversized file")
		}
		total += file.UncompressedSize64
		if total > MaxProfileTotalSize {
			return errors.New("account archive expands beyond the size limit")
		}
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if regularFiles[parent] {
				return errors.New("account archive has a file/directory path conflict")
			}
		}
	}
	return nil
}

func transientProfilePath(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		switch strings.ToLower(part) {
		case "cache2", "startupcache", "shader-cache", "thumbnails", "minidumps", "crashes", "parent.lock", ".parentlock", "lock", "compatibility.ini":
			return true
		}
	}
	return false
}

type archiveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// WriteProfileArchive requires the caller to hold the profile's session lease.
func (m *Manager) WriteProfileArchive(ctx context.Context, writer *zip.Writer, profile string) error {
	directory, err := m.ProfilePath(profile)
	if err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("read Playground profile: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Playground profile is not a regular directory")
	}
	var total int64
	count, hasCookies := 0, false
	returnErr := filepath.WalkDir(directory, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filePath == directory {
			return nil
		}
		rel, err := filepath.Rel(directory, filePath)
		if err != nil {
			return err
		}
		if transientProfilePath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Playground profile contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("Playground profile contains a special file")
		}
		count++
		total += info.Size()
		if count > MaxProfileFiles || info.Size() > MaxProfileFileSize || total > MaxProfileTotalSize {
			return errors.New("Playground profile exceeds the archive size limit")
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()
		out, err := writer.Create("profiles/" + profile + "/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		n, err := io.Copy(out, archiveContextReader{ctx, io.LimitReader(file, info.Size()+1)})
		if err != nil {
			return err
		}
		if n != info.Size() {
			return errors.New("Playground profile changed during export")
		}
		if rel == "cookies.sqlite" && n > 0 {
			hasCookies = true
		}
		return nil
	})
	if returnErr != nil {
		return returnErr
	}
	if !hasCookies {
		return errors.New("Playground browser profile has no cookie database; authorize the account before exporting")
	}
	return nil
}

// RestoreProfileArchive always allocates a new UUID and never overwrites an
// existing profile. Failed extraction removes only that newly created directory.
func (m *Manager) RestoreProfileArchive(ctx context.Context, archive *zip.Reader, source string) (profile string, err error) {
	if _, err = m.ProfilePath(source); err != nil {
		return "", err
	}
	if err = ValidateProfileArchive(archive); err != nil {
		return "", err
	}
	prefix := "profiles/" + source + "/"
	hasCookies := false
	for _, file := range archive.File {
		if file.Name == prefix+"cookies.sqlite" && file.UncompressedSize64 > 0 && file.Mode().IsRegular() {
			hasCookies = true
		}
	}
	if !hasCookies {
		return "", errors.New("archive is missing the Playground browser profile cookie database")
	}
	profile = uuid.NewString()
	directory, err := m.ProfilePath(profile)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		return "", err
	}
	if err = os.Mkdir(directory, 0700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = m.RemoveProfile(profile)
		}
	}()
	for _, file := range archive.File {
		if !strings.HasPrefix(file.Name, prefix) || file.FileInfo().IsDir() {
			continue
		}
		rel := strings.TrimPrefix(file.Name, prefix)
		if transientProfilePath(rel) {
			continue
		}
		destination := filepath.Join(directory, filepath.FromSlash(rel))
		if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return profile, err
		}
		var input io.ReadCloser
		input, err = file.Open()
		if err != nil {
			return profile, err
		}
		var output *os.File
		output, err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			return profile, err
		}
		var n int64
		n, err = io.Copy(output, archiveContextReader{ctx, io.LimitReader(input, int64(file.UncompressedSize64)+1)})
		err = errors.Join(err, input.Close(), output.Close())
		if err != nil {
			return profile, err
		}
		if n != int64(file.UncompressedSize64) {
			return profile, errors.New("account archive file size mismatch")
		}
	}
	return profile, nil
}
