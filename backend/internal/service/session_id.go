package service

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ClientIdentityIDs contains the client-provided conversation identifiers that
// are persisted with usage logs. Empty values mean that the request did not
// provide that identifier.
type ClientIdentityIDs struct {
	SessionID string
	ThreadID  string
	WindowID  string
}

const clientIdentityIDsContextKey = "client_identity_ids"
const resolvedClientIdentityIDsContextKey = "resolved_client_identity_ids"

// maxPersistedSessionIDLength bounds the persisted client session identifier to the
// usage_logs.session_id column width (VARCHAR(255)). Longer values are rejected so
// distinct identifiers can never alias through truncation.
const maxPersistedSessionIDLength = 255

// clientSessionIDHeaders extends the OpenAI-compatible sticky-session signals with
// native protocol identifiers that are safe to persist but must not alter OpenAI
// scheduling behavior.
var clientSessionIDHeaders = append(
	append([]string(nil), explicitOpenAIHeaderSessionNames...),
	claudeCodeSessionHeader,
)

// ClaudeCodeSessionIDFromHeader returns the stable Claude Code conversation
// identifier carried by X-Claude-Code-Session-Id. It is intentionally exposed
// separately from ExtractClientSessionID: callers that use it for routing must
// make that scope explicit rather than accidentally changing every protocol's
// session semantics.
func ClaudeCodeSessionIDFromHeader(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return sanitizeSessionID(c.GetHeader(claudeCodeSessionHeader))
}

// ExtractClientSessionID resolves the explicit client-provided session identifier from
// request headers for usage-log correlation and returns it sanitized. It is
// protocol-agnostic and shared by every gateway handler so all supported protocols
// record session_id through one seam. Returns "" when no valid identifier is present.
//
// This value feeds only usage_logs.session_id persistence. It does NOT affect sticky
// routing, account selection, request_id semantics, or upstream prompt caching, which
// keep their own (intentionally broader) session-signal resolution.
func ExtractClientSessionID(c *gin.Context) string {
	return ExtractClientIdentityIDs(c).SessionID
}

// ExtractClientIdentityIDs resolves the three stable Codex identity fields from
// flat headers and x-codex-turn-metadata. Flat headers take precedence.
func ExtractClientIdentityIDs(c *gin.Context) ClientIdentityIDs {
	var ids ClientIdentityIDs
	if c == nil || c.Request == nil {
		return ids
	}
	if resolved, ok := c.Get(resolvedClientIdentityIDsContextKey); ok {
		if cached, ok := resolved.(ClientIdentityIDs); ok {
			return cached
		}
	}
	for _, header := range clientSessionIDHeaders {
		if sessionID := sanitizeSessionID(c.GetHeader(header)); sessionID != "" {
			ids.SessionID = sessionID
			break
		}
	}
	if isGrokRequestContext(c) {
		if ids.SessionID == "" {
			ids.SessionID = sanitizeSessionID(c.GetHeader(grokConversationIDHeader))
		}
	}
	ids.ThreadID = sanitizeSessionID(c.GetHeader("thread-id"))
	if ids.ThreadID == "" {
		ids.ThreadID = sanitizeSessionID(c.GetHeader("x-codex-thread-id"))
	}
	ids.WindowID = sanitizeSessionID(c.GetHeader("x-codex-window-id"))
	if ids.SessionID == "" || ids.ThreadID == "" || ids.WindowID == "" {
		var metadata map[string]any
		if json.Unmarshal([]byte(c.GetHeader("x-codex-turn-metadata")), &metadata) == nil {
			if ids.SessionID == "" {
				if value, ok := metadata["session_id"].(string); ok {
					ids.SessionID = sanitizeSessionID(value)
				}
			}
			if ids.ThreadID == "" {
				if value, ok := metadata["thread_id"].(string); ok {
					ids.ThreadID = sanitizeSessionID(value)
				}
			}
			if ids.WindowID == "" {
				if value, ok := metadata["window_id"].(string); ok {
					ids.WindowID = sanitizeSessionID(value)
				}
			}
		}
	}
	if staged, ok := c.Get(clientIdentityIDsContextKey); ok {
		if bodyIDs, ok := staged.(ClientIdentityIDs); ok {
			if ids.SessionID == "" {
				ids.SessionID = bodyIDs.SessionID
			}
			if ids.ThreadID == "" {
				ids.ThreadID = bodyIDs.ThreadID
			}
			if ids.WindowID == "" {
				ids.WindowID = bodyIDs.WindowID
			}
		}
	}
	c.Set(resolvedClientIdentityIDsContextKey, ids)
	return ids
}

// StageClientIdentityIDsFromBody extracts identity fields from the small
// client_metadata portion of an already-read JSON request body. It is a
// fallback for Codex clients that send identifiers in the body instead of
// request headers; header values remain authoritative.
func StageClientIdentityIDsFromBody(c *gin.Context, body []byte) {
	if c == nil || len(body) == 0 {
		return
	}
	var ids ClientIdentityIDs
	ids.SessionID = bodyIdentityField(body, "session_id", "client_metadata.session_id")
	ids.ThreadID = bodyIdentityField(body, "thread_id", "client_metadata.thread_id")
	ids.WindowID = bodyIdentityField(body, "window_id", "client_metadata.x-codex-window-id", "client_metadata.window_id")
	metadata := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	if metadata.Exists() && metadata.Type == gjson.String {
		var embedded map[string]any
		if json.Unmarshal([]byte(metadata.String()), &embedded) == nil {
			if ids.SessionID == "" {
				if value, ok := embedded["session_id"].(string); ok {
					ids.SessionID = sanitizeSessionID(value)
				}
			}
			if ids.ThreadID == "" {
				if value, ok := embedded["thread_id"].(string); ok {
					ids.ThreadID = sanitizeSessionID(value)
				}
			}
			if ids.WindowID == "" {
				if value, ok := embedded["window_id"].(string); ok {
					ids.WindowID = sanitizeSessionID(value)
				}
			}
		}
	}
	// Always replace the snapshot so a later WebSocket turn cannot inherit
	// identifiers from an earlier turn that did not include them.
	c.Set(clientIdentityIDsContextKey, ids)
	c.Set(resolvedClientIdentityIDsContextKey, nil)
}

func bodyIdentityField(body []byte, paths ...string) string {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if value.Exists() && value.Type == gjson.String {
			if sanitized := sanitizeSessionID(value.String()); sanitized != "" {
				return sanitized
			}
		}
	}
	return ""
}

// sanitizeSessionID normalizes a raw client-supplied session identifier for safe
// persistence: it trims surrounding whitespace, rejects the value outright if it
// contains any control character (CR/LF/tab/NUL/…) so a log- or header-injection style
// payload cannot slip into stored correlation data, and rejects values longer than
// the DB column bound. Absent or invalid input yields "".
func sanitizeSessionID(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	count := 0
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			// An explicit correlation id never legitimately contains control
			// characters; drop the whole value rather than persist a mangled or
			// partially-injected identifier.
			return ""
		}
		count++
		if count > maxPersistedSessionIDLength {
			return ""
		}
	}
	return trimmed
}
