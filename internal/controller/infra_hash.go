package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

const (
	// AnnotationInfraHash is the annotation key for the infrastructure configuration hash
	// This is used to trigger pod restarts when infrastructure config changes
	AnnotationInfraHash = "varnish.io/infra-hash"
)

// InfrastructureConfig represents the infrastructure configuration that requires
// pod restart when changed (as opposed to hot-reloadable config changes)
type InfrastructureConfig struct {
	// GatewayImage is the container image for the gateway pods
	GatewayImage string

	// VarnishdExtraArgs are additional arguments passed to varnishd
	VarnishdExtraArgs []string

	// Logging configuration for varnish
	Logging *gatewayparamsv1alpha1.VarnishLogging

	// ImagePullSecrets for pulling the gateway image
	ImagePullSecrets []string

	// HasTLS indicates whether HTTPS listeners exist. Adding/removing an HTTPS
	// listener changes the varnishd listen args and requires a pod restart.
	// Individual cert ref changes are handled by the TLS file watcher without restart.
	HasTLS bool

	// ExtraVolumes are additional volumes to add to the varnish pod
	ExtraVolumes []corev1.Volume

	// ExtraVolumeMounts are additional volume mounts for the main container
	ExtraVolumeMounts []corev1.VolumeMount

	// ExtraInitContainers are additional init containers to run before the main container
	ExtraInitContainers []corev1.Container
}

// ComputeHash returns a deterministic SHA256 hash of the infrastructure configuration
// The hash is used to detect when infrastructure changes require a pod restart
func (c *InfrastructureConfig) ComputeHash() string {
	h := sha256.New()

	// Include image
	h.Write([]byte(c.GatewayImage))
	h.Write([]byte{0}) // separator

	// Include varnishd extra args (sorted for determinism)
	sortedArgs := make([]string, len(c.VarnishdExtraArgs))
	copy(sortedArgs, c.VarnishdExtraArgs)
	sort.Strings(sortedArgs)
	h.Write([]byte(strings.Join(sortedArgs, "\x00")))
	h.Write([]byte{0}) // separator

	// Include logging config
	if c.Logging != nil {
		h.Write([]byte(c.Logging.Mode))
		h.Write([]byte{0})
		h.Write([]byte(c.Logging.Format))
		h.Write([]byte{0})
		h.Write([]byte(c.Logging.Image))
		h.Write([]byte{0})
		// Include extra args (sorted for determinism)
		sortedLogArgs := make([]string, len(c.Logging.ExtraArgs))
		copy(sortedLogArgs, c.Logging.ExtraArgs)
		sort.Strings(sortedLogArgs)
		h.Write([]byte(strings.Join(sortedLogArgs, "\x00")))
		h.Write([]byte{0})
	}

	// Include image pull secrets (sorted for determinism)
	sortedSecrets := make([]string, len(c.ImagePullSecrets))
	copy(sortedSecrets, c.ImagePullSecrets)
	sort.Strings(sortedSecrets)
	h.Write([]byte(strings.Join(sortedSecrets, "\x00")))
	h.Write([]byte{0}) // separator

	// Include TLS flag (adding/removing HTTPS listeners changes listen args)
	if c.HasTLS {
		h.Write([]byte("tls"))
	}
	h.Write([]byte{0}) // separator

	// Include extra volumes, mounts, and init containers via JSON marshal.
	// JSON marshaling of corev1 struct types is deterministic (struct field order).
	if len(c.ExtraVolumes) > 0 {
		data, _ := json.Marshal(c.ExtraVolumes)
		h.Write(data)
	}
	h.Write([]byte{0})
	if len(c.ExtraVolumeMounts) > 0 {
		data, _ := json.Marshal(c.ExtraVolumeMounts)
		h.Write(data)
	}
	h.Write([]byte{0})
	if len(c.ExtraInitContainers) > 0 {
		data, _ := json.Marshal(c.ExtraInitContainers)
		h.Write(data)
	}

	return hex.EncodeToString(h.Sum(nil))
}
