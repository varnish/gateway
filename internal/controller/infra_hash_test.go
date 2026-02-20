package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

func TestInfrastructureConfig_ComputeHash(t *testing.T) {
	tests := []struct {
		name   string
		config InfrastructureConfig
		want   string // We'll check for stability, not exact value
	}{
		{
			name: "basic config",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging:           nil,
				ImagePullSecrets:  nil,
			},
		},
		{
			name: "with logging",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging: &gatewayparamsv1alpha1.VarnishLogging{
					Mode:   "varnishlog",
					Format: "%h %l %u %t",
					Image:  "ghcr.io/varnish/gateway:v1.0.0",
				},
				ImagePullSecrets: nil,
			},
		},
		{
			name: "with image pull secrets",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging:           nil,
				ImagePullSecrets:  []string{"secret1", "secret2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.ComputeHash()
			if got == "" {
				t.Error("ComputeHash() returned empty string")
			}
			// Hash should be 64 characters (SHA256 hex encoded)
			if len(got) != 64 {
				t.Errorf("ComputeHash() returned hash with length %d, want 64", len(got))
			}
		})
	}
}

func TestInfrastructureConfig_HashStability(t *testing.T) {
	// Same config should produce same hash
	config := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k", "-p", "thread_pools=2"},
		Logging: &gatewayparamsv1alpha1.VarnishLogging{
			Mode:   "varnishlog",
			Format: "%h %l %u %t",
			Image:  "ghcr.io/varnish/gateway:v1.0.0",
		},
		ImagePullSecrets: []string{"secret1", "secret2"},
	}

	hash1 := config.ComputeHash()
	hash2 := config.ComputeHash()

	if hash1 != hash2 {
		t.Errorf("ComputeHash() not stable: got %s and %s", hash1, hash2)
	}
}

func TestInfrastructureConfig_HashChangesOnChange(t *testing.T) {
	baseConfig := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
		Logging:           nil,
		ImagePullSecrets:  nil,
	}
	baseHash := baseConfig.ComputeHash()

	tests := []struct {
		name   string
		config InfrastructureConfig
	}{
		{
			name: "different image",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v2.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging:           nil,
				ImagePullSecrets:  nil,
			},
		},
		{
			name: "different varnishd args",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=256k"},
				Logging:           nil,
				ImagePullSecrets:  nil,
			},
		},
		{
			name: "added logging",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging: &gatewayparamsv1alpha1.VarnishLogging{
					Mode:   "varnishncsa",
					Format: "%h %l %u %t",
					Image:  "ghcr.io/varnish/gateway:v1.0.0",
				},
				ImagePullSecrets: nil,
			},
		},
		{
			name: "different image pull secrets",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				Logging:           nil,
				ImagePullSecrets:  []string{"different-secret"},
			},
		},
		{
			name: "extra volumes added",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				ExtraVolumes: []corev1.Volume{
					{Name: "vmod-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		},
		{
			name: "extra volume mounts added",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				ExtraVolumeMounts: []corev1.VolumeMount{
					{Name: "vmod-vol", MountPath: "/usr/lib/varnish/vmods"},
				},
			},
		},
		{
			name: "extra init containers added",
			config: InfrastructureConfig{
				GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
				VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k"},
				ExtraInitContainers: []corev1.Container{
					{Name: "vmod-loader", Image: "busybox:latest"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changedHash := tt.config.ComputeHash()
			if changedHash == baseHash {
				t.Errorf("ComputeHash() did not change after modifying %s", tt.name)
			}
		})
	}
}

func TestInfrastructureConfig_ArgOrderDoesNotAffectHash(t *testing.T) {
	// Args in different order should produce same hash (they get sorted)
	config1 := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: []string{"-p", "thread_pool_stack=160k", "-p", "thread_pools=2"},
		Logging:           nil,
		ImagePullSecrets:  nil,
	}

	config2 := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: []string{"-p", "thread_pools=2", "-p", "thread_pool_stack=160k"},
		Logging:           nil,
		ImagePullSecrets:  nil,
	}

	hash1 := config1.ComputeHash()
	hash2 := config2.ComputeHash()

	if hash1 != hash2 {
		t.Errorf("ComputeHash() affected by arg order: %s != %s", hash1, hash2)
	}
}

func TestInfrastructureConfig_HasTLSChangesHash(t *testing.T) {
	noTLS := InfrastructureConfig{
		GatewayImage: "ghcr.io/varnish/gateway:v1.0.0",
		HasTLS:       false,
	}

	withTLS := InfrastructureConfig{
		GatewayImage: "ghcr.io/varnish/gateway:v1.0.0",
		HasTLS:       true,
	}

	if noTLS.ComputeHash() == withTLS.ComputeHash() {
		t.Error("HasTLS=true should produce different hash from HasTLS=false")
	}
}

func TestInfrastructureConfig_SecretOrderDoesNotAffectHash(t *testing.T) {
	// Secrets in different order should produce same hash (they get sorted)
	config1 := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: nil,
		Logging:           nil,
		ImagePullSecrets:  []string{"secret-a", "secret-b", "secret-c"},
	}

	config2 := InfrastructureConfig{
		GatewayImage:      "ghcr.io/varnish/gateway:v1.0.0",
		VarnishdExtraArgs: nil,
		Logging:           nil,
		ImagePullSecrets:  []string{"secret-c", "secret-a", "secret-b"},
	}

	hash1 := config1.ComputeHash()
	hash2 := config2.ComputeHash()

	if hash1 != hash2 {
		t.Errorf("ComputeHash() affected by secret order: %s != %s", hash1, hash2)
	}
}
