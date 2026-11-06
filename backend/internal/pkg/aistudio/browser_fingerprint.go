package aistudio

import "encoding/json"

// NewBrowserFingerprint shares the same version and configuration with the WAA runtime.
func NewBrowserFingerprint() (map[string]any, error) {
	return buildCamoufoxConfig(camoufoxFirefoxMajor, "en-US", "America/Los_Angeles")
}
func (s *StorageState) SetBrowserFingerprint(config map[string]any) error {
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	s.Fingerprint = raw
	return nil
}
