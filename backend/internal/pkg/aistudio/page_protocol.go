package aistudio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const firefoxInnerHeightDiff = 57
const publicDiscoveryUserAgent = DefaultUserAgent

func profileFromFingerprint(config map[string]any) (waa.Profile, string, string) {
	profile := waa.Profile{Window: map[string]any{}, Navigator: map[string]any{}, Screen: map[string]any{}, Document: map[string]any{}}
	for key, value := range config {
		scope, name, found := strings.Cut(key, ".")
		if !found {
			continue
		}
		switch scope {
		case "navigator":
			profile.Navigator[name] = value
		case "screen":
			profile.Screen[name] = value
		case "window":
			profile.Window[name] = value
		}
	}
	for _, name := range []string{"availLeft", "availTop"} {
		if _, ok := profile.Screen[name]; !ok {
			profile.Screen[name] = 0
		}
	}
	profile.Navigator["doNotTrack"] = "unspecified"
	profile.Navigator["globalPrivacyControl"] = false
	if width, ok := numberValue(profile.Window["outerWidth"]); ok {
		profile.Window["innerWidth"] = width
	}
	if height, ok := numberValue(profile.Window["outerHeight"]); ok {
		profile.Window["innerHeight"] = height - firefoxInnerHeightDiff
	}
	userAgent, _ := config["navigator.userAgent"].(string)
	if userAgent == "" {
		userAgent = publicDiscoveryUserAgent
	}
	acceptLanguage, _ := config["headers.Accept-Language"].(string)
	if acceptLanguage == "" {
		acceptLanguage = "en-US,en;q=0.5"
	}
	return profile, userAgent, acceptLanguage
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func encodeJSPBFields(raw []byte) ([]byte, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var output []byte
	for index, field := range fields {
		number := uint64(index + 1)
		trimmed := bytes.TrimSpace(field)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			continue
		}
		switch trimmed[0] {
		case '"':
			var text string
			if err := json.Unmarshal(trimmed, &text); err != nil {
				return nil, err
			}
			output = appendVarint(output, number<<3|2)
			output = appendVarint(output, uint64(len(text)))
			output = append(output, text...)
		case 't', 'f':
			output = appendVarint(output, number<<3)
			if trimmed[0] == 't' {
				output = appendVarint(output, 1)
			} else {
				output = appendVarint(output, 0)
			}
		default:
			var value int64
			if err := json.Unmarshal(trimmed, &value); err != nil {
				return nil, fmt.Errorf("字段 %d 不是整数: %s", number, trimmed)
			}
			output = appendVarint(output, number<<3)
			output = appendVarint(output, uint64(value))
		}
	}
	return output, nil
}

func appendVarint(output []byte, value uint64) []byte {
	for value >= 0x80 {
		output = append(output, byte(value)|0x80)
		value >>= 7
	}
	return append(output, byte(value))
}

// requiresTrustedScripts 判断页面 CSP 是否含 require-trusted-types-for 'script'
func requiresTrustedScripts(policies []string) bool {
	for _, policy := range policies {
		for _, directive := range strings.FieldsFunc(policy, func(character rune) bool { return character == ';' || character == ',' }) {
			fields := strings.Fields(directive)
			if len(fields) > 1 && strings.EqualFold(fields[0], "require-trusted-types-for") && slices.Contains(fields[1:], "'script'") {
				return true
			}
		}
	}
	return false
}

// pageTogglesPattern 匹配官网页面内联脚本写入的构建开关数组
var pageTogglesPattern = regexp.MustCompile(`window\["_F_toggles_default_MakerSuite"\] = \[([^\]]*)\]`)

// pageStringGlobalPattern 匹配官网页面内联脚本以 var 声明的构建字符串
var pageStringGlobalPattern = regexp.MustCompile(`var (_F_cssRowKey|_F_combinedSignature) = '([^']*)'`)

// pageGlobals 返回官网页面内联脚本声明的当前构建全局值
func pageGlobals(body []byte) map[string]any {
	globals := map[string]any{}
	if match := pageTogglesPattern.FindSubmatch(body); match != nil {
		toggles := []any{}
		for _, item := range strings.Split(string(match[1]), ",") {
			if value, err := strconv.ParseInt(strings.TrimSpace(item), 0, 64); err == nil {
				toggles = append(toggles, float64(value))
			}
		}
		globals["_F_toggles_default_MakerSuite"] = toggles
	}
	for _, match := range pageStringGlobalPattern.FindAllSubmatch(body, -1) {
		globals[string(match[1])] = string(match[2])
	}
	return globals
}
