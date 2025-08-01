/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package legacyspiffe

import (
	"testing"
	"time"

	"github.com/gravitational/teleport/lib/tbot/bot"
	"github.com/gravitational/teleport/lib/tbot/workloadidentity/workloadattest"
)

func ptr[T any](v T) *T {
	return &v
}

func TestSPIFFEWorkloadAPIService_YAML(t *testing.T) {
	t.Parallel()

	tests := []testYAMLCase[WorkloadAPIConfig]{
		{
			name: "full",
			in: WorkloadAPIConfig{
				Listen:     "unix:///var/run/spiffe.sock",
				JWTSVIDTTL: time.Minute * 5,
				Attestors: workloadattest.Config{
					Kubernetes: workloadattest.KubernetesAttestorConfig{
						Enabled: true,
						Kubelet: workloadattest.KubeletClientConfig{
							SecurePort: 12345,
							TokenPath:  "/path/to/token",
							CAPath:     "/path/to/ca.pem",
							SkipVerify: true,
							Anonymous:  true,
						},
					},
				},
				SVIDs: []SVIDRequestWithRules{
					{
						SVIDRequest: SVIDRequest{
							Path: "/foo",
							Hint: "hint",
							SANS: SVIDRequestSANs{
								DNS: []string{"example.com"},
								IP:  []string{"10.0.0.1", "10.42.0.1"},
							},
						},
						Rules: []SVIDRequestRule{
							{
								Unix: SVIDRequestRuleUnix{
									PID: ptr(100),
									UID: ptr(1000),
									GID: ptr(1234),
								},
							},
							{
								Unix: SVIDRequestRuleUnix{
									PID: ptr(100),
								},
								Kubernetes: SVIDRequestRuleKubernetes{
									Namespace:      "my-namespace",
									PodName:        "my-pod",
									ServiceAccount: "service-account",
								},
							},
						},
					},
				},
				CredentialLifetime: bot.CredentialLifetime{
					TTL:             1 * time.Minute,
					RenewalInterval: 30 * time.Second,
				},
			},
		},
		{
			name: "minimal",
			in: WorkloadAPIConfig{
				Listen: "unix:///var/run/spiffe.sock",
				SVIDs: []SVIDRequestWithRules{
					{
						SVIDRequest: SVIDRequest{
							Path: "/foo",
						},
					},
				},
			},
		},
	}
	testYAML(t, tests)
}

func TestSPIFFEWorkloadAPIService_CheckAndSetDefaults(t *testing.T) {
	t.Parallel()

	tests := []testCheckAndSetDefaultsCase[*WorkloadAPIConfig]{
		{
			name: "valid",
			in: func() *WorkloadAPIConfig {
				return &WorkloadAPIConfig{
					JWTSVIDTTL: time.Minute,
					Listen:     "unix:///var/run/spiffe.sock",
					SVIDs: []SVIDRequestWithRules{
						{
							SVIDRequest: SVIDRequest{
								Path: "/foo",
								Hint: "hint",
								SANS: SVIDRequestSANs{
									DNS: []string{"example.com"},
									IP:  []string{"10.0.0.1", "10.42.0.1"},
								},
							},
						},
					},
				}
			},
			want: &WorkloadAPIConfig{
				JWTSVIDTTL: time.Minute,
				Listen:     "unix:///var/run/spiffe.sock",
				SVIDs: []SVIDRequestWithRules{
					{
						SVIDRequest: SVIDRequest{
							Path: "/foo",
							Hint: "hint",
							SANS: SVIDRequestSANs{
								DNS: []string{"example.com"},
								IP:  []string{"10.0.0.1", "10.42.0.1"},
							},
						},
					},
				},
				Attestors: workloadattest.Config{
					Unix: workloadattest.UnixAttestorConfig{
						BinaryHashMaxSizeBytes: workloadattest.DefaultBinaryHashMaxBytes,
					},
				},
			},
		},
		{
			name: "missing path",
			in: func() *WorkloadAPIConfig {
				return &WorkloadAPIConfig{
					Listen: "unix:///var/run/spiffe.sock",
					SVIDs: []SVIDRequestWithRules{
						{
							SVIDRequest: SVIDRequest{
								Path: "",
								Hint: "hint",
								SANS: SVIDRequestSANs{
									DNS: []string{"example.com"},
									IP:  []string{"10.0.0.1", "10.42.0.1"},
								},
							},
						},
					},
				}
			},
			wantErr: "svid.path: should not be empty",
		},
		{
			name: "path missing leading slash",
			in: func() *WorkloadAPIConfig {
				return &WorkloadAPIConfig{
					Listen: "unix:///var/run/spiffe.sock",
					SVIDs: []SVIDRequestWithRules{
						{
							SVIDRequest: SVIDRequest{
								Path: "foo",
								Hint: "hint",
								SANS: SVIDRequestSANs{
									DNS: []string{"example.com"},
									IP:  []string{"10.0.0.1", "10.42.0.1"},
								},
							},
						},
					},
				}
			},
			wantErr: "svid.path: should be prefixed with /",
		},
		{
			name: "missing listen addr",
			in: func() *WorkloadAPIConfig {
				return &WorkloadAPIConfig{
					Listen: "",
					SVIDs: []SVIDRequestWithRules{
						{
							SVIDRequest: SVIDRequest{
								Path: "foo",
								Hint: "hint",
								SANS: SVIDRequestSANs{
									DNS: []string{"example.com"},
									IP:  []string{"10.0.0.1", "10.42.0.1"},
								},
							},
						},
					},
				}
			},
			wantErr: "listen: should not be empty",
		},
		{
			name: "invalid ip",
			in: func() *WorkloadAPIConfig {
				return &WorkloadAPIConfig{
					Listen: "unix:///var/run/spiffe.sock",
					SVIDs: []SVIDRequestWithRules{
						{
							SVIDRequest: SVIDRequest{
								Path: "/foo",
								Hint: "hint",
								SANS: SVIDRequestSANs{
									DNS: []string{"example.com"},
									IP:  []string{"invalid ip"},
								},
							},
						},
					},
				}
			},
			wantErr: "ip_sans[0]: invalid IP address",
		},
	}
	testCheckAndSetDefaults(t, tests)
}
