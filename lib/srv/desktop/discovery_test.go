/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
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

package desktop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth/authtest"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/service/servicecfg"
	"github.com/gravitational/teleport/lib/services"
	logutils "github.com/gravitational/teleport/lib/utils/log"
)

// TestDiscoveryLDAPFilter verifies that WindowsService produces a valid
// LDAP filter when given valid configuration.
func TestDiscoveryLDAPFilter(t *testing.T) {
	for _, test := range []struct {
		desc    string
		filters []string
		assert  require.ErrorAssertionFunc
	}{
		{
			desc:   "OK - no custom filters",
			assert: require.NoError,
		},
		{
			desc:    "OK - custom filters",
			filters: []string{"(computerName=test)", "(location=Oakland)"},
			assert:  require.NoError,
		},
		{
			desc:    "NOK - invalid custom filter",
			filters: []string{"invalid"},
			assert:  require.Error,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			s := new(WindowsService)
			filter := s.ldapSearchFilter(test.filters)
			_, err := ldap.CompileFilter(filter)
			test.assert(t, err)
		})
	}
}

func TestAppliesLDAPLabels(t *testing.T) {
	l := make(map[string]string)
	entry := ldap.NewEntry("CN=test,DC=example,DC=com", map[string][]string{
		attrDNSHostName:       {"foo.example.com"},
		attrName:              {"foo"},
		attrOS:                {"Windows Server"},
		attrOSVersion:         {"6.1"},
		attrDistinguishedName: {"CN=foo,OU=IT,DC=goteleport,DC=com"},
		attrCommonName:        {"foo"},
		"bar":                 {"baz"},
		"quux":                {""},
	})

	s := new(WindowsService)
	s.applyLabelsFromLDAP(entry, l, &servicecfg.LDAPDiscoveryConfig{
		BaseDN:          "*",
		LabelAttributes: []string{"bar"},
	})

	// check default labels
	require.Equal(t, types.OriginDynamic, l[types.OriginLabel])
	require.Equal(t, "foo.example.com", l[types.DiscoveryLabelWindowsDNSHostName])
	require.Equal(t, "foo", l[types.DiscoveryLabelWindowsComputerName])
	require.Equal(t, "Windows Server", l[types.DiscoveryLabelWindowsOS])
	require.Equal(t, "6.1", l[types.DiscoveryLabelWindowsOSVersion])

	// check OU label
	require.Equal(t, "OU=IT,DC=goteleport,DC=com", l[types.DiscoveryLabelWindowsOU])

	// check custom labels
	require.Equal(t, "baz", l["ldap/bar"])
	require.Empty(t, l["ldap/quux"])
}

func TestLabelsDomainControllers(t *testing.T) {
	s := &WindowsService{}
	for _, test := range []struct {
		desc   string
		entry  *ldap.Entry
		assert require.BoolAssertionFunc
	}{
		{
			desc: "DC",
			entry: ldap.NewEntry("CN=test,DC=example,DC=com", map[string][]string{
				attrPrimaryGroupID: {writableDomainControllerGroupID},
			}),
			assert: require.True,
		},
		{
			desc: "RODC",
			entry: ldap.NewEntry("CN=test,DC=example,DC=com", map[string][]string{
				attrPrimaryGroupID: {readOnlyDomainControllerGroupID},
			}),
			assert: require.True,
		},
		{
			desc: "computer",
			entry: ldap.NewEntry("CN=test,DC=example,DC=com", map[string][]string{
				attrPrimaryGroupID: {"515"},
			}),
			assert: require.False,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			l := make(map[string]string)
			s.applyLabelsFromLDAP(test.entry, l, new(servicecfg.LDAPDiscoveryConfig))

			b, _ := strconv.ParseBool(l[types.DiscoveryLabelWindowsIsDomainController])
			test.assert(t, b)
		})
	}
}

// TestDNSErrors verifies that errors are handled quickly
// and do not block discovery for too long.
func TestDNSErrors(t *testing.T) {
	s := &WindowsService{
		cfg: WindowsServiceConfig{
			Logger:               slog.New(logutils.NewSlogTextHandler(io.Discard, logutils.SlogTextHandlerConfig{})),
			Clock:                clockwork.NewRealClock(),
			ConnectedProxyGetter: reversetunnel.NewConnectedProxyGetter(),
		},
		dnsResolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return nil, errors.New("this resolver always fails")
			},
		},
	}

	start := time.Now()
	_, err := s.lookupDesktop(context.Background(), "$invalid hostname")
	require.Less(t, time.Since(start), dnsQueryTimeout-1*time.Second)
	require.Error(t, err)
}

func TestDynamicWindowsDiscovery(t *testing.T) {
	t.Parallel()
	authServer, err := authtest.NewAuthServer(authtest.AuthServerConfig{
		ClusterName: "test",
		Dir:         t.TempDir(),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, authServer.Close())
	})

	tlsServer, err := authServer.NewTestTLSServer()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tlsServer.Close())
	})

	client, err := tlsServer.NewClient(authtest.TestServerID(types.RoleWindowsDesktop, "test-host-id"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	dynamicWindowsClient := client.DynamicDesktopClient()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, testCase := range []struct {
		name     string
		labels   map[string]string
		expected int
	}{
		{
			name:     "no labels",
			expected: 0,
		},
		{
			name:     "no matching labels",
			labels:   map[string]string{"xyz": "abc"},
			expected: 0,
		},
		{
			name:     "matching labels",
			labels:   map[string]string{"foo": "bar"},
			expected: 1,
		},
		{
			name:     "matching wildcard labels",
			labels:   map[string]string{"abc": "abc"},
			expected: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s := &WindowsService{
				cfg: WindowsServiceConfig{
					Heartbeat: HeartbeatConfig{
						HostUUID: "1234",
					},
					Logger:               slog.New(logutils.NewSlogTextHandler(io.Discard, logutils.SlogTextHandlerConfig{})),
					Clock:                clockwork.NewFakeClock(),
					AuthClient:           client,
					AccessPoint:          client,
					ConnectedProxyGetter: reversetunnel.NewConnectedProxyGetter(),
					ResourceMatchers: []services.ResourceMatcher{{
						Labels: types.Labels{
							"foo": {"bar"},
						},
					}, {
						Labels: types.Labels{
							"abc": {"*"},
						},
					}},
				},
				dnsResolver: &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						return nil, errors.New("this resolver always fails")
					},
				},
			}
			reconciler, err := s.startDynamicReconciler(ctx)
			require.NoError(t, err)
			t.Cleanup(func() {
				reconciler.Close()
				require.NoError(t, authServer.AuthServer.DeleteAllWindowsDesktops(ctx))
				var key string
				for {
					page, next, err := authServer.AuthServer.ListDynamicWindowsDesktops(ctx, 0, key)
					require.NoError(t, err)
					for _, dwd := range page {
						require.NoError(t, authServer.AuthServer.DeleteDynamicWindowsDesktop(ctx, dwd.GetName()))
					}
					if next == "" {
						break
					}
					key = next
				}
			})

			desktop, err := types.NewDynamicWindowsDesktopV1("test", testCase.labels, types.DynamicWindowsDesktopSpecV1{
				Addr: "addr",
			})
			require.NoError(t, err)

			_, err = dynamicWindowsClient.CreateDynamicWindowsDesktop(ctx, desktop)
			require.NoError(t, err)

			require.EventuallyWithT(t, func(t *assert.CollectT) {
				desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
				if !assert.NoError(t, err) {
					return
				}
				if !assert.Len(t, desktops, testCase.expected) {
					return
				}

				if testCase.expected > 0 {
					assert.Equal(t, desktop.GetName(), desktops[0].GetName())
					assert.Equal(t, desktop.GetAddr(), desktops[0].GetAddr())
				}
			}, 5*time.Second, 50*time.Millisecond)

			desktop.Spec.Addr = "addr2"
			_, err = dynamicWindowsClient.UpsertDynamicWindowsDesktop(ctx, desktop)
			require.NoError(t, err)

			require.EventuallyWithT(t, func(t *assert.CollectT) {
				desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
				if !assert.NoError(t, err) {
					return
				}
				if !assert.Len(t, desktops, testCase.expected) {
					return
				}
				if testCase.expected > 0 {
					assert.Equal(t, desktop.GetName(), desktops[0].GetName())
					assert.Equal(t, desktop.GetAddr(), desktops[0].GetAddr())
				}
			}, 5*time.Second, 50*time.Millisecond)

			require.NoError(t, dynamicWindowsClient.DeleteDynamicWindowsDesktop(ctx, "test"))

			require.EventuallyWithT(t, func(t *assert.CollectT) {
				desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
				assert.NoError(t, err)
				assert.Empty(t, desktops)
			}, 5*time.Second, 50*time.Millisecond)
		})
	}
}

func TestDynamicWindowsDiscoveryExpiry(t *testing.T) {
	authServer, err := authtest.NewAuthServer(authtest.AuthServerConfig{
		ClusterName: "test",
		Dir:         t.TempDir(),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, authServer.Close())
	})

	tlsServer, err := authServer.NewTestTLSServer()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tlsServer.Close())
	})

	client, err := tlsServer.NewClient(authtest.TestServerID(types.RoleWindowsDesktop, "test-host-id"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	dynamicWindowsClient := client.DynamicDesktopClient()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clock := clockwork.NewFakeClock()
	s := &WindowsService{
		cfg: WindowsServiceConfig{
			Heartbeat: HeartbeatConfig{
				HostUUID: "1234",
			},
			Logger:      slog.New(logutils.NewSlogTextHandler(io.Discard, logutils.SlogTextHandlerConfig{})),
			Clock:       clock,
			AuthClient:  client,
			AccessPoint: client,
			ResourceMatchers: []services.ResourceMatcher{{
				Labels: types.Labels{
					"foo": {"bar"},
				},
			}},
		},
		dnsResolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return nil, errors.New("this resolver always fails")
			},
		},
	}
	_, err = s.startDynamicReconciler(ctx)
	require.NoError(t, err)

	desktop, err := types.NewDynamicWindowsDesktopV1("test", map[string]string{
		"foo": "bar",
	}, types.DynamicWindowsDesktopSpecV1{
		Addr: "addr",
	})
	require.NoError(t, err)

	_, err = dynamicWindowsClient.CreateDynamicWindowsDesktop(ctx, desktop)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(t *assert.CollectT) {
		desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
		require.NoError(t, err)
		require.Len(t, desktops, 1)
		require.Equal(t, "test", desktops[0].GetName())
	}, 5*time.Second, 50*time.Millisecond)

	err = client.DeleteWindowsDesktop(ctx, s.cfg.Heartbeat.HostUUID, "test")
	require.NoError(t, err)
	desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
	require.NoError(t, err)
	require.Empty(t, desktops)

	clock.Advance(5 * time.Minute)

	require.EventuallyWithT(t, func(t *assert.CollectT) {
		desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
		require.NoError(t, err)
		require.Len(t, desktops, 1)
		require.Equal(t, "test", desktops[0].GetName())
	}, 5*time.Second, 50*time.Millisecond)
}
