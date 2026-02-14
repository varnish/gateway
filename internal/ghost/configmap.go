package ghost

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ExtractRoutingConfig extracts routing.json from ConfigMap data.
func ExtractRoutingConfig(cm *corev1.ConfigMap) ([]byte, error) {
	data, ok := cm.Data["routing.json"]
	if !ok {
		return nil, fmt.Errorf("routing.json not found in ConfigMap")
	}
	return []byte(data), nil
}

// ValidateRoutingConfig validates routing.json version field.
func ValidateRoutingConfig(data []byte) error {
	var check struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &check); err != nil {
		return fmt.Errorf("json.Unmarshal: %w", err)
	}
	if check.Version != 2 {
		return fmt.Errorf("unsupported version: %d", check.Version)
	}
	return nil
}
