package aistudio

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseCookies(t *testing.T) {
	raw := "SAPISID=test_sapisid; __Secure-1PAPISID=test_1p; SSID=test_ssid"
	cookies := ParseCookies(raw)
	require.Equal(t, "test_sapisid", cookies["SAPISID"])
	require.Equal(t, "test_1p", cookies["__Secure-1PAPISID"])
	require.Equal(t, "test_ssid", cookies["SSID"])
}

func TestBuildAuthorizationHeader(t *testing.T) {
	raw := "SAPISID=dummy_sapisid_12345; __Secure-1PAPISID=dummy_1p_12345"
	now := time.Unix(1730000000, 0)
	header, err := BuildAuthorizationHeader(raw, AIStudioOrigin, now)
	require.NoError(t, err)
	require.Contains(t, header, "SAPISIDHASH 1730000000_")
	require.Contains(t, header, "SAPISID1PHASH 1730000000_")
}

func TestBuildAuthorizationHeader_MissingSAPISID(t *testing.T) {
	raw := "OTHER=val"
	_, err := BuildAuthorizationHeader(raw, AIStudioOrigin, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "SAPISID")
}

func TestParseCookieHeaderErrorDoesNotExposeCredential(t *testing.T) {
	_, err := ParseCookieText("SAPISID=private-cookie-value; private-malformed-credential")
	require.EqualError(t, err, "Cookie 请求头第 2 项格式无效")
	require.NotContains(t, err.Error(), "private-cookie-value")
	require.NotContains(t, err.Error(), "private-malformed-credential")
}
