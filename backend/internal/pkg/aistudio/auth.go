package aistudio

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	AIStudioOrigin          = "https://aistudio.google.com"
	AIStudioReferer         = "https://aistudio.google.com/"
	MakerSuiteRPCBase       = "https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/"
	JSONProtobufContentType = "application/json+protobuf"
	DefaultUserAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:152.0) Gecko/20100101 Firefox/152.0"
	aiStudioOrigin          = AIStudioOrigin
)

var signatureCookies = [...]struct {
	label string
	name  string
}{
	{label: "SAPISIDHASH", name: "SAPISID"},
	{label: "SAPISID1PHASH", name: "__Secure-1PAPISID"},
	{label: "SAPISID3PHASH", name: "__Secure-3PAPISID"},
}

// ParseCookies parses a raw Cookie header or semi-colon separated cookie string.
func ParseCookies(raw string) map[string]string {
	result := make(map[string]string)
	pairs := strings.Split(raw, ";")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}

// BuildAuthorizationHeader calculates the SAPISIDHASH headers from cookies.
func BuildAuthorizationHeader(cookieStr string, origin string, now time.Time) (string, error) {
	if origin == "" {
		origin = AIStudioOrigin
	}
	cookies := ParseCookies(cookieStr)
	timestamp := now.Unix()

	var parts []string
	// Always look for at least SAPISID
	for _, item := range signatureCookies {
		val, exists := cookies[item.name]
		if !exists || val == "" {
			continue
		}
		source := fmt.Sprintf("%d %s %s", timestamp, val, origin)
		digest := sha1.Sum([]byte(source))
		hash := hex.EncodeToString(digest[:])
		parts = append(parts, fmt.Sprintf("%s %d_%s", item.label, timestamp, hash))
	}

	if len(parts) == 0 {
		return "", errors.New("cookie does not contain SAPISID or __Secure-1PAPISID")
	}

	return strings.Join(parts, " "), nil
}

// BuildRequestHeaders creates standard browser headers required by MakerSuiteService.
func BuildRequestHeaders(cookieStr string, userAgent string) (http.Header, error) {
	authHeader, err := BuildAuthorizationHeader(cookieStr, AIStudioOrigin, time.Now())
	if err != nil {
		return nil, err
	}

	ua := userAgent
	if strings.TrimSpace(ua) == "" {
		ua = DefaultUserAgent
	}

	header := make(http.Header)
	header.Set("Authorization", authHeader)
	header.Set("Origin", AIStudioOrigin)
	header.Set("Referer", AIStudioReferer)
	header.Set("Content-Type", JSONProtobufContentType)
	header.Set("X-Goog-AuthUser", "0")
	header.Set("User-Agent", ua)
	header.Set("Cookie", cookieStr)
	header.Set("X-User-Agent", "grpc-web-javascript/0.1")
	return header, nil
}
