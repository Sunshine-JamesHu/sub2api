package camoufox

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func testProfileArchive(t *testing.T, entries map[string]string) *zip.Reader {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		file, err := writer.Create(name)
		require.NoError(t, err)
		_, err = file.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	return archive
}

func TestProfileArchiveRoundTrip(t *testing.T) {
	manager := NewManager("", t.TempDir())
	profile := uuid.NewString()
	directory, err := manager.ProfilePath(profile)
	require.NoError(t, err)
	for name, content := range map[string]string{"cookies.sqlite": "synthetic-cookie-db", "storage/default/example/ls/data.sqlite": "local-storage", "key4.db": "key-material", "cache2/entries/temp": "cache", "parent.lock": "lock"} {
		file := filepath.Join(directory, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
		require.NoError(t, os.WriteFile(file, []byte(content), 0600))
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	require.NoError(t, manager.WriteProfileArchive(context.Background(), writer, profile))
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	require.Len(t, archive.File, 3)
	restored, err := manager.RestoreProfileArchive(context.Background(), archive, profile)
	require.NoError(t, err)
	require.NotEqual(t, profile, restored)
	destination, err := manager.ProfilePath(restored)
	require.NoError(t, err)
	for _, name := range []string{"cookies.sqlite", "storage/default/example/ls/data.sqlite", "key4.db"} {
		original, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(name)))
		require.NoError(t, err)
		copied, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		require.NoError(t, err)
		require.Equal(t, original, copied)
	}
	require.NoFileExists(t, filepath.Join(destination, "parent.lock"))
}

func TestProfileArchiveRejectsUnsafeEntries(t *testing.T) {
	profile := uuid.NewString()
	for _, name := range []string{"../escape", "/absolute", "profiles/" + profile + "/../escape", "profiles/" + profile + "/C:escape", "profiles\\" + profile + "\\cookie", "profiles/" + profile + "/CON", "profiles/" + profile + "/file. ", "profiles/not-a-uuid/cookies.sqlite", "unknown.json"} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, ValidateProfileArchive(testProfileArchive(t, map[string]string{name: "bad"})))
		})
	}
	archive := testProfileArchive(t, map[string]string{"profiles/" + profile + "/cookies.sqlite": "cookie", "profiles/" + profile + "/COOKIES.SQLITE": "duplicate"})
	require.ErrorContains(t, ValidateProfileArchive(archive), "duplicate")
	archive = testProfileArchive(t, map[string]string{"profiles/" + profile + "/storage": "file", "profiles/" + profile + "/storage/child": "conflict"})
	require.ErrorContains(t, ValidateProfileArchive(archive), "conflict")
	archive = testProfileArchive(t, map[string]string{"profiles/" + profile + "/cookies.sqlite": "cookie"})
	archive.File[0].SetMode(os.ModeSymlink | 0600)
	require.ErrorContains(t, ValidateProfileArchive(archive), "link")
	archive.File[0].SetMode(0600)
	archive.File[0].UncompressedSize64 = MaxProfileFileSize + 1
	require.ErrorContains(t, ValidateProfileArchive(archive), "oversized")
	archive = testProfileArchive(t, map[string]string{"profiles/" + profile + "/cookies.sqlite": "cookie", "profiles/" + profile + "/key4.db": "key", "profiles/" + profile + "/cert9.db": "cert", "profiles/" + profile + "/prefs.js": "prefs", "profiles/" + profile + "/storage.sqlite": "storage"})
	for _, entry := range archive.File {
		entry.UncompressedSize64 = MaxProfileFileSize
	}
	require.ErrorContains(t, ValidateProfileArchive(archive), "expands")
}

func TestProfileArchiveMissingAndFailedRestoreLeaveNoProfiles(t *testing.T) {
	manager := NewManager("", t.TempDir())
	profile := uuid.NewString()
	_, err := manager.RestoreProfileArchive(context.Background(), testProfileArchive(t, map[string]string{"accounts.json": "{}"}), profile)
	require.ErrorContains(t, err, "cookie database")
	archive := testProfileArchive(t, map[string]string{"profiles/" + profile + "/cookies.sqlite": "cookie"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = manager.RestoreProfileArchive(ctx, archive, profile)
	require.ErrorIs(t, err, context.Canceled)
	entries, err := os.ReadDir(manager.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
