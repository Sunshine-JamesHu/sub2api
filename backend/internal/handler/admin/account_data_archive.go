package admin

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type limitedArchiveWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedArchiveWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("account archive exceeds the 256 MiB compressed size limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func (h *AccountHandler) exportDataArchive(c *gin.Context, payload DataPayload) {
	file, err := os.CreateTemp("", "sub2api-accounts-*.zip")
	if err != nil {
		response.Error(c, 500, "Unable to prepare account archive")
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	writer := zip.NewWriter(&limitedArchiveWriter{writer: file, remaining: camoufox.MaxProfileArchiveSize})
	manifest, err := writer.Create("accounts.json")
	if err == nil {
		var raw []byte
		raw, err = json.Marshal(payload)
		if err == nil && len(raw) > camoufox.MaxArchiveManifestSize {
			err = errors.New("account archive manifest exceeds 8 MiB")
		}
		if err == nil {
			_, err = manifest.Write(raw)
		}
	}
	seen := make(map[string]bool)
	for _, account := range payload.Accounts {
		if err != nil {
			break
		}
		if account.Type != service.AccountTypePlayground {
			continue
		}
		profile, _ := account.Credentials["playground_profile_id"].(string)
		if seen[profile] {
			continue
		}
		seen[profile] = true
		err = h.playgroundBrowser.ExportProfile(c.Request.Context(), writer, profile)
	}
	err = errors.Join(err, writer.Close())
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && info.Size() > camoufox.MaxProfileArchiveSize {
			err = errors.New("account archive exceeds the 256 MiB compressed size limit")
		}
	}
	if err == nil {
		var archive *zip.ReadCloser
		archive, err = zip.OpenReader(file.Name())
		if err == nil {
			err = camoufox.ValidateProfileArchive(&archive.Reader)
			archive.Close()
		}
	}
	if err != nil {
		response.Error(c, 500, "Unable to export Playground archive: "+err.Error())
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		response.Error(c, 500, "Unable to read account archive")
		return
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", `attachment; filename="sub2api-accounts-`+time.Now().UTC().Format("20060102-150405")+`.zip"`)
	c.Header("X-Skipped-Shadows", strconv.Itoa(payload.SkippedShadows))
	c.Header("Cache-Control", "no-store")
	http.ServeContent(c.Writer, c.Request, "accounts.zip", time.Time{}, file)
}

func (h *AccountHandler) importDataArchive(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, camoufox.MaxProfileArchiveSize+(1<<20))
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
		response.BadRequest(c, "Invalid account archive upload (maximum 256 MiB)")
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		response.BadRequest(c, "Account archive file is required")
		return
	}
	defer file.Close()
	if header.Size > camoufox.MaxProfileArchiveSize {
		response.BadRequest(c, "Account archive exceeds 256 MiB")
		return
	}
	archive, err := zip.NewReader(file, header.Size)
	if err == nil {
		err = camoufox.ValidateProfileArchive(archive)
	}
	if err != nil {
		response.BadRequest(c, "Invalid account archive: "+err.Error())
		return
	}
	var payload DataPayload
	found := false
	for _, entry := range archive.File {
		if entry.Name != "accounts.json" {
			continue
		}
		found = true
		input, openErr := entry.Open()
		if openErr != nil {
			err = openErr
			break
		}
		raw, readErr := io.ReadAll(io.LimitReader(input, camoufox.MaxArchiveManifestSize+1))
		input.Close()
		if readErr != nil {
			err = readErr
			break
		}
		err = json.Unmarshal(raw, &payload)
	}
	if !found || err != nil {
		response.BadRequest(c, "Account archive must contain a valid accounts.json")
		return
	}
	if err = validateDataHeader(payload); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	// Check every profile reference before importing any proxies or accounts.
	for _, account := range payload.Accounts {
		if account.Type != service.AccountTypePlayground {
			continue
		}
		profile, _ := account.Credentials["playground_profile_id"].(string)
		email, _ := account.Credentials["playground_email"].(string)
		if account.Platform != service.PlatformGemini || profile == "" || !strings.Contains(email, "@") {
			response.BadRequest(c, "Playground archive is missing profile identity")
			return
		}
		present := false
		for _, entry := range archive.File {
			if entry.Name == "profiles/"+profile+"/cookies.sqlite" && entry.UncompressedSize64 > 0 {
				present = true
				break
			}
		}
		if !present {
			response.BadRequest(c, "Account archive is missing a referenced Playground profile")
			return
		}
	}
	skip := true
	if raw := c.Request.FormValue("skip_default_group_bind"); raw != "" {
		skip, err = strconv.ParseBool(raw)
		if err != nil {
			response.BadRequest(c, "Invalid skip_default_group_bind")
			return
		}
	}
	req := DataImportRequest{Data: payload, SkipDefaultGroupBind: &skip}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		response.BadRequest(c, "Unable to read account archive")
		return
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		response.BadRequest(c, "Unable to hash account archive")
		return
	}
	idempotencyPayload := struct {
		Request DataImportRequest `json:"request"`
		Digest  string            `json:"archive_sha256"`
	}{req, hex.EncodeToString(digest.Sum(nil))}
	executeAdminIdempotentJSON(c, "admin.accounts.import_archive", idempotencyPayload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.importDataWithArchive(ctx, req, archive)
	})
}

func (h *AccountHandler) preparePlaygroundArchiveAccount(ctx context.Context, account DataAccount, proxyID *int64, archive *zip.Reader) (map[string]any, func(), error) {
	if archive == nil {
		return nil, nil, errors.New("Playground accounts require a ZIP export containing the browser profile")
	}
	if account.Platform != service.PlatformGemini {
		return nil, nil, errors.New("Playground accounts require the Gemini platform")
	}
	profile, _ := account.Credentials["playground_profile_id"].(string)
	email, _ := account.Credentials["playground_email"].(string)
	token, rollback, err := h.playgroundBrowser.ImportProfile(ctx, archive, profile, email, proxyID)
	if err != nil {
		return nil, nil, fmt.Errorf("restore Playground profile: %w", err)
	}
	credentials := make(map[string]any, len(account.Credentials)+1)
	for key, value := range account.Credentials {
		credentials[key] = value
	}
	credentials["playground_login_token"] = token
	return credentials, rollback, nil
}
