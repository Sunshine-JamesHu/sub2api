package geminiweb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Model struct {
	Name        string `json:"name"`
	ID          string `json:"web_model_id"`
	DisplayName string `json:"displayName"`
	Capacity    int    `json:"capacity"`
	Number      int    `json:"model_number"`
}

func at(value any, indexes ...int) any {
	for _, index := range indexes {
		list, ok := value.([]any)
		if !ok || index < 0 || index >= len(list) {
			return nil
		}
		value = list[index]
	}
	return value
}
func str(value any) string  { text, _ := value.(string); return text }
func integer(value any) int { number, _ := value.(float64); return int(number) }

var modelID = regexp.MustCompile(`^[a-f0-9]{16}$`)
var slugPattern = regexp.MustCompile(`[^a-z0-9.]+`)

// Only models explicitly reported by this account are routable. No guessed IDs.
func ParseModels(raw []byte) ([]Model, error) {
	result := map[string]Model{}
	err := decodeFrames(bytes.NewReader(raw), func(frame []any) error {
		if str(at(frame, 0)) != "wrb.fr" || str(at(frame, 1)) != "otAQ7b" {
			return nil
		}
		var status []any
		if err := json.Unmarshal([]byte(str(at(frame, 2))), &status); err != nil {
			return fmt.Errorf("decode Gemini Web model status: %w", err)
		}
		entries, _ := at(status, 15).([]any)
		capacity := 1
		// Known subscription-specific protocol IDs identify capacity without
		// mistaking an individual model entry's unrelated fields for account tier.
		for _, entry := range entries {
			switch str(at(entry, 0)) {
			case "e6fa609c3fa255c0", "56fdd199312815e2", "8c46e95b1a07cecc", "95a4c0abcba24364":
				capacity = 2
			}
		}
		for _, entry := range entries {
			id := str(at(entry, 0))
			if !modelID.MatchString(id) {
				continue
			}
			display := ""
			for _, index := range []int{11, 19, 1} {
				if display = str(at(entry, index)); display != "" {
					break
				}
			}
			family := strings.ToLower(str(at(entry, 1)))
			if family == "" {
				family = strings.ToLower(str(at(entry, 10)))
			}
			if family == "" {
				family = strings.ToLower(display)
			}
			name := ""
			switch id {
			case "9d8ca3786ebdfbea", "e6fa609c3fa255c0":
				name = "gemini-pro"
			case "fbb127bbb056c959", "56fdd199312815e2":
				name = "gemini-flash"
			case "cf41b0e0dd7d53e5", "8c46e95b1a07cecc":
				name = "gemini-flash-lite"
			default:
				switch {
				case strings.Contains(family, "flash") && strings.Contains(family, "lite"):
					name = "gemini-flash-lite"
				case strings.Contains(family, "flash"):
					name = "gemini-flash"
				case family == "pro":
					name = "gemini-pro"
				default:
					name = "gemini-" + strings.Trim(slugPattern.ReplaceAllString(family, "-"), "-")
				}
			}
			if name == "gemini-" || len(name) > 70 {
				continue
			}
			number := integer(at(entry, 17))
			if number == 0 {
				number = integer(at(entry, 9))
			}
			if number == 0 {
				switch name {
				case "gemini-pro":
					number = 3
				case "gemini-flash":
					number = 1
				case "gemini-flash-lite":
					number = 6
				}
			}
			if number <= 0 || number > 1000 {
				continue
			}
			if _, exists := result[name]; !exists {
				result[name] = Model{Name: name, ID: id, DisplayName: display, Capacity: capacity, Number: number}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Gemini Web returned no recognized models; finish website onboarding or check the account's access")
	}
	models := make([]Model, 0, len(result))
	for _, model := range result {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

func ResolveModel(models []Model, name string) (Model, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
	for _, model := range models {
		if name == model.Name || name == "gemini-"+strings.ToLower(strings.ReplaceAll(model.DisplayName, " ", "-")) {
			return model, nil
		}
	}
	// Historical official names are explicit aliases for the website's model
	// families. Image/preview/unknown model names must be configured by the admin.
	aliases := map[string]string{"gemini-2.5-pro": "gemini-pro", "gemini-3-pro": "gemini-pro", "gemini-3.1-pro": "gemini-pro", "gemini-2.5-flash": "gemini-flash", "gemini-3-flash": "gemini-flash", "gemini-2.5-flash-lite": "gemini-flash-lite", "gemini-3-flash-lite": "gemini-flash-lite"}
	if alias := aliases[name]; alias != "" {
		for _, model := range models {
			if model.Name == alias {
				return model, nil
			}
		}
	}
	return Model{}, &Error{Status: 400, Message: fmt.Sprintf("Gemini Web model %q is unavailable for this account; sync models or configure a model mapping", name)}
}
