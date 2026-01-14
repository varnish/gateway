package vrun

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// Config holds varnishd configuration options.
type Config struct {
	// Required - used internally by BuildArgs
	WorkDir   string // Secret file path is derived from this
	AdminPort int    // -M localhost:<port>

	// Optional
	VarnishDir  string // -n argument, empty for default
	LicensePath string // -L argument, empty to skip

	// Generic - user provides raw argument values
	Listen  []string          // -a values, e.g. ":8080,http", ":443,https", "/path.sock,http"
	Storage []string          // -s values, e.g. "malloc,256m", "file,/path,10G"
	Params  map[string]string // -p parameters as name -> value
}

// BuildArgs constructs varnishd command line arguments from Config.
func BuildArgs(cfg *Config) []string {
	args := make([]string, 0)

	if cfg.LicensePath != "" {
		args = append(args, "-L", cfg.LicensePath)
	}

	secretPath := filepath.Join(cfg.WorkDir, "secret")
	args = append(args, "-S", secretPath)
	args = append(args, "-M", fmt.Sprintf("localhost:%d", cfg.AdminPort))

	if cfg.VarnishDir != "" {
		args = append(args, "-n", cfg.VarnishDir)
	}

	args = append(args, "-F")
	args = append(args, "-f", "")

	for _, l := range cfg.Listen {
		args = append(args, "-a", l)
	}

	for _, s := range cfg.Storage {
		args = append(args, "-s", s)
	}

	for k, v := range cfg.Params {
		args = append(args, "-p", fmt.Sprintf("%s=%s", k, v))
	}

	return args
}

// GetParamName extracts the Varnish parameter name from the yaml struct tag.
// Returns the parameter name (without ",omitempty" suffix) or empty string if no yaml tag exists.
func GetParamName(field reflect.StructField) string {
	yamlTag := field.Tag.Get("yaml")
	if yamlTag == "" || yamlTag == "-" {
		return ""
	}
	// Remove ",omitempty" or other tag options
	if idx := strings.Index(yamlTag, ","); idx != -1 {
		return yamlTag[:idx]
	}
	return yamlTag
}
