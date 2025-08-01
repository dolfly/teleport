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

// Package service implements teleport running service, takes care
// of initialization, cleanup and shutdown procedures
package service

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/google/renameio/v2"
	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/client/webclient"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	accessgraphsecretsv1pb "github.com/gravitational/teleport/api/gen/proto/go/teleport/accessgraph/v1"
	integrationpb "github.com/gravitational/teleport/api/gen/proto/go/teleport/integration/v1"
	kubeproto "github.com/gravitational/teleport/api/gen/proto/go/teleport/kube/v1"
	transportpb "github.com/gravitational/teleport/api/gen/proto/go/teleport/transport/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/discoveryconfig"
	apievents "github.com/gravitational/teleport/api/types/events"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/aws"
	"github.com/gravitational/teleport/api/utils/grpc/interceptors"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/api/utils/retryutils"
	apisshutils "github.com/gravitational/teleport/api/utils/sshutils"
	"github.com/gravitational/teleport/entitlements"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/agentless"
	"github.com/gravitational/teleport/lib/auditd"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/accesspoint"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/auth/keygen"
	"github.com/gravitational/teleport/lib/auth/keystore"
	"github.com/gravitational/teleport/lib/auth/machineid/machineidv1"
	"github.com/gravitational/teleport/lib/auth/recordingencryption"
	"github.com/gravitational/teleport/lib/auth/state"
	"github.com/gravitational/teleport/lib/auth/storage"
	"github.com/gravitational/teleport/lib/auth/summarizer"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/automaticupgrades"
	autoupdate "github.com/gravitational/teleport/lib/autoupdate/agent"
	"github.com/gravitational/teleport/lib/autoupdate/rollout"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/dynamo"
	_ "github.com/gravitational/teleport/lib/backend/etcdbk"
	"github.com/gravitational/teleport/lib/backend/firestore"
	"github.com/gravitational/teleport/lib/backend/kubernetes"
	_ "github.com/gravitational/teleport/lib/backend/lite"
	_ "github.com/gravitational/teleport/lib/backend/pgbk"
	"github.com/gravitational/teleport/lib/bpf"
	"github.com/gravitational/teleport/lib/cache"
	pgrepl "github.com/gravitational/teleport/lib/client/db/postgres/repl"
	dbrepl "github.com/gravitational/teleport/lib/client/db/repl"
	"github.com/gravitational/teleport/lib/cloud"
	"github.com/gravitational/teleport/lib/cloud/awsconfig"
	"github.com/gravitational/teleport/lib/cloud/gcp"
	"github.com/gravitational/teleport/lib/cloud/imds"
	awsimds "github.com/gravitational/teleport/lib/cloud/imds/aws"
	"github.com/gravitational/teleport/lib/cloud/imds/azure"
	gcpimds "github.com/gravitational/teleport/lib/cloud/imds/gcp"
	oracleimds "github.com/gravitational/teleport/lib/cloud/imds/oracle"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/events/athena"
	"github.com/gravitational/teleport/lib/events/azsessions"
	"github.com/gravitational/teleport/lib/events/dynamoevents"
	"github.com/gravitational/teleport/lib/events/filesessions"
	"github.com/gravitational/teleport/lib/events/firestoreevents"
	"github.com/gravitational/teleport/lib/events/gcssessions"
	"github.com/gravitational/teleport/lib/events/pgevents"
	"github.com/gravitational/teleport/lib/events/s3sessions"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/integrations/awsoidc"
	"github.com/gravitational/teleport/lib/integrations/awsra"
	"github.com/gravitational/teleport/lib/integrations/externalauditstorage"
	"github.com/gravitational/teleport/lib/inventory"
	"github.com/gravitational/teleport/lib/joinserver"
	kubegrpc "github.com/gravitational/teleport/lib/kube/grpc"
	kubeproxy "github.com/gravitational/teleport/lib/kube/proxy"
	"github.com/gravitational/teleport/lib/labels"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/multiplexer"
	"github.com/gravitational/teleport/lib/observability/tracing"
	"github.com/gravitational/teleport/lib/openssh"
	"github.com/gravitational/teleport/lib/pam"
	"github.com/gravitational/teleport/lib/plugin"
	"github.com/gravitational/teleport/lib/proxy"
	"github.com/gravitational/teleport/lib/proxy/peer"
	peerquic "github.com/gravitational/teleport/lib/proxy/peer/quic"
	"github.com/gravitational/teleport/lib/resumption"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/reversetunnelclient"
	secretsscannerproxy "github.com/gravitational/teleport/lib/secretsscanner/proxy"
	"github.com/gravitational/teleport/lib/selinux"
	"github.com/gravitational/teleport/lib/service/servicecfg"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/services/expiry"
	"github.com/gravitational/teleport/lib/services/local"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/srv/alpnproxy"
	alpnproxyauth "github.com/gravitational/teleport/lib/srv/alpnproxy/auth"
	alpncommon "github.com/gravitational/teleport/lib/srv/alpnproxy/common"
	"github.com/gravitational/teleport/lib/srv/app"
	"github.com/gravitational/teleport/lib/srv/db"
	"github.com/gravitational/teleport/lib/srv/desktop"
	"github.com/gravitational/teleport/lib/srv/ingress"
	"github.com/gravitational/teleport/lib/srv/mcp"
	"github.com/gravitational/teleport/lib/srv/regular"
	"github.com/gravitational/teleport/lib/srv/transport/transportv1"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/system"
	"github.com/gravitational/teleport/lib/tlsca"
	usagereporter "github.com/gravitational/teleport/lib/usagereporter/teleport"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/cert"
	"github.com/gravitational/teleport/lib/utils/hostid"
	logutils "github.com/gravitational/teleport/lib/utils/log"
	"github.com/gravitational/teleport/lib/versioncontrol/endpoint"
	uw "github.com/gravitational/teleport/lib/versioncontrol/upgradewindow"
	"github.com/gravitational/teleport/lib/web"
	webapp "github.com/gravitational/teleport/lib/web/app"
)

const (
	// AuthIdentityEvent is generated when the Auth Servers identity has been
	// initialized in the backend.
	AuthIdentityEvent = "AuthIdentity"

	// InstanceIdentityEvent is generated by the supervisor when the instance-level
	// identity has been registered with the Auth server.
	InstanceIdentityEvent = "InstanceIdentity"

	// ProxyIdentityEvent is generated by the supervisor when the proxy's
	// identity has been registered with the Auth Server.
	ProxyIdentityEvent = "ProxyIdentity"

	// RelayIdentityEvent is generated after the Teleport agent has successfully
	// joined a cluster or reused credentials to connect to the control plane as
	// a relay service.
	RelayIdentityEvent = "RelayIdentityEvent"

	// SSHIdentityEvent is generated when node's identity has been registered
	// with the Auth Server.
	SSHIdentityEvent = "SSHIdentity"

	// KubeIdentityEvent is generated by the supervisor when the kubernetes
	// service's identity has been registered with the Auth Server.
	KubeIdentityEvent = "KubeIdentity"

	// AppsIdentityEvent is generated when the identity of the application proxy
	// service has been registered with the Auth Server.
	AppsIdentityEvent = "AppsIdentity"

	// DatabasesIdentityEvent is generated when the identity of the database
	// proxy service has been registered with the auth server.
	DatabasesIdentityEvent = "DatabasesIdentity"

	// WindowsDesktopIdentityEvent is generated by the supervisor when the
	// windows desktop service's identity has been registered with the Auth
	// Server.
	WindowsDesktopIdentityEvent = "WindowsDesktopIdentity"

	// DiscoveryIdentityEvent is generated when the identity of the
	DiscoveryIdentityEvent = "DiscoveryIdentityEvent"

	// AuthTLSReady is generated when the Auth Server has initialized the
	// TLS Mutual Auth endpoint and is ready to start accepting connections.
	AuthTLSReady = "AuthTLSReady"

	// ProxyWebServerReady is generated when the proxy has initialized the web
	// server and is ready to start accepting connections.
	ProxyWebServerReady = "ProxyWebServerReady"

	// ProxyReverseTunnelReady is generated when the proxy has initialized the
	// reverse tunnel server and is ready to start accepting connections.
	ProxyReverseTunnelReady = "ProxyReverseTunnelReady"

	// DebugAppReady is generated when the debugging application has been started
	// and is ready to serve requests.
	DebugAppReady = "DebugAppReady"

	// ProxyAgentPoolReady is generated when the proxy has initialized the
	// remote cluster watcher (to spawn reverse tunnels) and is ready to start
	// accepting connections.
	ProxyAgentPoolReady = "ProxyAgentPoolReady"

	// ProxySSHReady is generated when the proxy has initialized a SSH server
	// and is ready to start accepting connections.
	ProxySSHReady = "ProxySSHReady"

	// RelayReady is generated when a relay service is ready to accept
	// connections.
	RelayReady = "RelayReady"

	// NodeSSHReady is generated when the Teleport node has initialized a SSH server
	// and is ready to start accepting SSH connections.
	NodeSSHReady = "NodeReady"

	// KubernetesReady is generated when the kubernetes service has been initialized.
	KubernetesReady = "KubernetesReady"

	// AppsReady is generated when the Teleport app proxy service is ready to
	// start accepting connections.
	AppsReady = "AppsReady"

	// DatabasesReady is generated when the Teleport database proxy service
	// is ready to start accepting connections.
	DatabasesReady = "DatabasesReady"

	// MetricsReady is generated when the Teleport metrics service is ready to
	// start accepting connections.
	MetricsReady = "MetricsReady"

	// WindowsDesktopReady is generated when the Teleport windows desktop
	// service is ready to start accepting connections.
	WindowsDesktopReady = "WindowsDesktopReady"

	// TracingReady is generated when the Teleport tracing service is ready to
	// start exporting spans.
	TracingReady = "TracingReady"

	// InstanceReady is generated when the teleport instance control handle has
	// been set up.
	InstanceReady = "InstanceReady"

	// DiscoveryReady is generated when the Teleport discovery service
	// is ready to start accepting connections.
	DiscoveryReady = "DiscoveryReady"

	// TeleportExitEvent is generated when the Teleport process begins closing
	// all listening sockets and exiting.
	TeleportExitEvent = "TeleportExit"

	// TeleportTerminatingEvent is generated when the Teleport process receives
	// a signal to shut down. It's always generated as part of the process
	// lifecycle and it's always generated before TeleportExitEvent, but there
	// might be some configured delay between this event and the
	// TeleportExitEvent signaling the actual beginning of the shut down
	// procedures. It should be used to advertise the fact that the Teleport
	// instance is going to shut down at some near time in the future, not to
	// reduce the functionality of services - i.e., it's perfectly fine for
	// services to ignore this event altogether, and nothing should get closed
	// as a result of this event.
	TeleportTerminatingEvent = "TeleportTerminating"

	// TeleportPhaseChangeEvent is generated to indicate that the CA rotation
	// phase has been updated, used in tests.
	TeleportPhaseChangeEvent = "TeleportPhaseChange"

	// TeleportCredentialsUpdatedEvent is generated to indicate that credentials
	// have been reissued due to a CA rotation or a principals or DNS names
	// change, used in tests.
	TeleportCredentialsUpdatedEvent = "TeleportCredentialsUpdated"

	// TeleportReadyEvent is generated to signal that all teleport
	// internal components have started successfully.
	TeleportReadyEvent = "TeleportReady"

	// ServiceExitedWithErrorEvent is emitted whenever a service
	// has exited with an error, the payload includes the error
	ServiceExitedWithErrorEvent = "ServiceExitedWithError"

	// TeleportDegradedEvent is emitted whenever a service is operating in a
	// degraded manner.
	TeleportDegradedEvent = "TeleportDegraded"

	// TeleportOKEvent is emitted whenever a service is operating normally.
	TeleportOKEvent = "TeleportOKEvent"
)

func newConnector(clientIdentity, serverIdentity *state.Identity) (*Connector, error) {
	clientState, err := newConnectorState(clientIdentity)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	serverState := clientState
	if serverIdentity != clientIdentity {
		s, err := newConnectorState(serverIdentity)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		serverState = s
	}
	c := &Connector{
		clusterName: clientIdentity.ClusterName,
		hostID:      clientIdentity.ID.HostUUID,
		role:        clientIdentity.ID.Role,
	}
	c.clientState.Store(clientState)
	c.serverState.Store(serverState)
	return c, nil
}

func newConnectorState(identity *state.Identity) (*connectorState, error) {
	state := &connectorState{
		identity: identity,
	}
	if identity.Cert != nil {
		hostCheckers, err := apisshutils.ParseAuthorizedKeys(identity.SSHCACertBytes)
		if err != nil {
			return nil, trace.Wrap(err, "parsing SSH host CAs")
		}
		state.hostCheckers = hostCheckers

		state.sshCert = identity.Cert
		state.sshCertSigner = identity.KeySigner
	}
	if identity.HasTLSConfig() {
		tlsCert, err := keys.X509KeyPair(identity.TLSCertBytes, identity.KeyBytes)
		if err != nil {
			return nil, trace.Wrap(err, "parsing X.509 certificate")
		}
		tlsCert.Leaf = identity.XCert
		certPool := x509.NewCertPool()
		for j := range identity.TLSCACertsBytes {
			parsedCert, err := tlsca.ParseCertificatePEM(identity.TLSCACertsBytes[j])
			if err != nil {
				return nil, trace.Wrap(err, "parsing X.509 host CA")
			}
			certPool.AddCert(parsedCert)
		}
		state.tlsCert = &tlsCert
		state.pool = certPool
	}
	return state, nil
}

// connectorState contains immutable state (generally derived from a
// [*state.Identity]) suitable for sharing behind an atomic pointer.
type connectorState struct {
	identity *state.Identity

	// tlsCert is the TLS client certificate for the identity, with Signer and
	// Leaf filled.
	tlsCert *tls.Certificate
	// pool contains the host CA certificates trusted by the identity.
	pool *x509.CertPool

	// sshCert is the SSH certificate associated with the identity.
	sshCert *ssh.Certificate
	// sshCertSigner is a [ssh.Signer] presenting the sshCert certificate as its
	// public key.
	sshCertSigner ssh.Signer
	// hostCheckers contains the (non-certificate) public keys that make up the
	// host CA trusted by the identity.
	hostCheckers []ssh.PublicKey
}

// Connector has all resources process needs to connect to other parts of the
// cluster: client and identity.
type Connector struct {
	clusterName string
	hostID      string
	role        types.SystemRole

	// clientState contains the current connector state for outbound connections
	// to the cluster.
	clientState atomic.Pointer[connectorState]
	// serverState contains the current connector state for inbound connections
	// from the cluster.
	serverState atomic.Pointer[connectorState]

	// Client is an authenticated client intended to use the credentials in
	// clientState (unless it's a client shared from some other connector as
	// signified by ReusedClient).
	Client *authclient.Client

	// ReusedClient, if true, indicates that the client reference is owned by
	// a different connector and should not be closed.
	ReusedClient bool
}

func (c *Connector) ClusterName() string {
	return c.clusterName
}

func (c *Connector) HostID() string {
	return c.hostID
}

func (c *Connector) Role() types.SystemRole {
	return c.role
}

// ClientGetCertificate returns the current credentials for outgoing TLS
// connections to other cluster components.
func (c *Connector) ClientGetCertificate() (*tls.Certificate, error) {
	tlsCert := c.clientState.Load().tlsCert
	if tlsCert == nil {
		return nil, trace.NotFound("no TLS credentials setup for this identity")
	}
	return tlsCert, nil
}

// ClientGetPool returns a pool with the trusted X.509/TLS signers from the host
// CA of the local cluster, as known by the connector.
func (c *Connector) ClientGetPool() (*x509.CertPool, error) {
	roots := c.clientState.Load().pool
	if roots == nil {
		return nil, trace.NotFound("no TLS credentials setup for this identity")
	}
	return roots, nil
}

// ClientAuthMethods returns the [ssh.AuthMethod]s that should be used for
// outgoing SSH connections to other cluster components (the Proxy Service,
// almost surely).
func (c *Connector) ClientAuthMethods() []ssh.AuthMethod {
	return []ssh.AuthMethod{
		ssh.PublicKeysCallback(func() (signers []ssh.Signer, err error) {
			sshCertSigner := c.clientState.Load().sshCertSigner
			if sshCertSigner == nil {
				return nil, nil
			}
			return []ssh.Signer{sshCertSigner}, nil
		}),
	}
}

func (c *Connector) clientIdentityString() string {
	return c.clientState.Load().identity.String()
}

func (c *Connector) clientSSHClientConfig(fips bool) (*ssh.ClientConfig, error) {
	hostKeyCallback, err := apisshutils.NewHostKeyCallback(
		apisshutils.HostKeyCallbackConfig{
			GetHostCheckers: func() ([]ssh.PublicKey, error) {
				return c.clientState.Load().hostCheckers, nil
			},
			FIPS: fips,
		})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &ssh.ClientConfig{
		User: c.hostID,
		Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(func() (signers []ssh.Signer, err error) {
			return []ssh.Signer{c.clientState.Load().sshCertSigner}, nil
		})},
		HostKeyCallback: hostKeyCallback,
		Timeout:         apidefaults.DefaultIOTimeout,
	}, nil
}

// ServerTLSConfig returns a new server-side [*tls.Config] that presents the
// connector's credentials as its certificate. The returned tls.Config doesn't
// request or trust any client certificates, so the caller is responsible for
// configuring it.
func (c *Connector) ServerTLSConfig(cipherSuites []uint16) (*tls.Config, error) {
	conf := utils.TLSConfig(cipherSuites)
	conf.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return c.serverGetCertificate()
	}
	return conf, nil
}

// ServerGetHostSigners returns the [ssh.Signer]s that should be used as host
// keys for incoming SSH connections.
func (c *Connector) ServerGetHostSigners() []ssh.Signer {
	sshCertSigner := c.serverState.Load().sshCertSigner
	if sshCertSigner == nil {
		return nil
	}
	return []ssh.Signer{sshCertSigner}
}

func (c *Connector) ServerGetValidPrincipals() []string {
	// TODO(espadolini): get rid of this function after refactoring the two
	// integration tests that use it
	sshCert := c.serverState.Load().sshCert
	if sshCert == nil {
		return nil
	}
	return slices.Clone(sshCert.ValidPrincipals)
}

func (c *Connector) serverGetCertificate() (*tls.Certificate, error) {
	tlsCert := c.serverState.Load().tlsCert
	if tlsCert == nil {
		return nil, trace.NotFound("no TLS credentials setup for this identity")
	}
	return tlsCert, nil
}

func (c *Connector) getPROXYSigner(clock clockwork.Clock, allowDowngrade bool) (multiplexer.PROXYHeaderSigner, error) {
	proxySigner, err := multiplexer.NewPROXYSigner(c.clusterName, c.serverGetCertificate, clock, allowDowngrade)
	if err != nil {
		return nil, trace.Wrap(err, "could not create PROXY signer")
	}
	return proxySigner, nil
}

// TunnelProxyResolver if non-nil, indicates that the client is connected to the Auth Server
// through the reverse SSH tunnel proxy
func (c *Connector) TunnelProxyResolver() reversetunnelclient.Resolver {
	if c.Client == nil || c.Client.Dialer() == nil {
		return nil
	}

	switch dialer := c.Client.Dialer().(type) {
	case *reversetunnelclient.TunnelAuthDialer:
		return dialer.Resolver
	default:
		return nil
	}
}

// UseTunnel indicates if the client is connected directly to the Auth Server
// (false) or through the proxy (true).
func (c *Connector) UseTunnel() bool {
	return c.TunnelProxyResolver() != nil
}

// Close closes resources associated with connector
func (c *Connector) Close() error {
	if c.Client != nil && !c.ReusedClient {
		return c.Client.Close()
	}
	return nil
}

// TeleportProcess structure holds the state of the Teleport daemon, controlling
// execution and configuration of the teleport services: ssh, auth and proxy.
type TeleportProcess struct {
	Clock clockwork.Clock
	sync.Mutex
	Supervisor
	Config *servicecfg.Config

	// PluginsRegistry handles plugin registrations with Teleport services
	PluginRegistry plugin.Registry

	// localAuth has local auth server listed in case if this process
	// has started with auth server role enabled
	localAuth *auth.Server
	// backend is the process' backend
	backend backend.Backend
	// auditLog is the initialized audit log
	auditLog events.AuditLogSessionStreamer

	// inventorySetupDelay lets us inject a one-time delay in the makeInventoryControlStream
	// method that helps reduce log spam in the event of slow instance cert acquisition.
	inventorySetupDelay sync.Once

	// inventoryHandle is the downstream inventory control handle for this instance.
	inventoryHandle inventory.DownstreamHandle

	// instanceConnector contains the instance-level connector. this is created asynchronously
	// and may not exist for some time if cert migrations are necessary.
	instanceConnector *Connector

	// instanceConnectorReady is closed when the isntance client becomes available.
	instanceConnectorReady chan struct{}

	// instanceConnectorReadyOnce protects instanceConnectorReady from double-close.
	instanceConnectorReadyOnce sync.Once

	// instanceRoles is the collection of enabled service roles (excludes things like "admin"
	// and "instance" which aren't true user-facing services). The values in this mapping are
	// the names of the associated identity events for these roles.
	instanceRoles map[types.SystemRole]string

	// hostedPluginRoles is the collection of dynamically enabled service roles. This element
	// behaves equivalent to instanceRoles except that while instance roles are static assignments
	// set up when the teleport process starts, hosted plugin roles are dynamically assigned by
	// runtime configuration, and may not necessarily be present on the instance cert.
	hostedPluginRoles map[types.SystemRole]string

	// connectors is a list of connected clients and their identities
	connectors map[types.SystemRole]*Connector

	// registeredListeners keeps track of all listeners created by the process
	// used to pass listeners to child processes during live reload
	registeredListeners []registeredListener
	// importedDescriptors is a list of imported file descriptors
	// passed by the parent process
	importedDescriptors []*servicecfg.FileDescriptor
	// listenersClosed is a flag that indicates that the process should not open
	// new listeners (for instance, because we're shutting down and we've already
	// closed all the listeners)
	listenersClosed bool

	// forkedTeleportCount is the count of forked Teleport child processes
	// currently active, as spawned by SIGHUP or SIGUSR2.
	forkedTeleportCount atomic.Int32

	// storage is a server local storage
	storage *storage.ProcessStorage

	// rotationCache is a TTL cache for GetRotation, since it might get called
	// frequently if the agent is heartbeating multiple resources. Keys are
	// [types.SystemRole], values are [*types.Rotation].
	rotationCache *utils.FnCache

	// id is a process id - used to identify different processes
	// during in-process reloads.
	id string

	// logger is a process-local slog.Logger.
	logger *slog.Logger

	// reporter is used to report some in memory stats
	reporter *backend.Reporter

	// clusterFeatures contain flags for supported and unsupported features.
	clusterFeatures proto.Features

	// authSubjectiveAddr is the peer address of this process as seen by the auth
	// server during the most recent ping (may be empty).
	authSubjectiveAddr string

	// cloudLabels is a set of labels imported from a cloud provider and shared between
	// services.
	cloudLabels labels.Importer
	// TracingProvider is the provider to be used for exporting traces. In the event
	// that tracing is disabled this will be a no-op provider that drops all spans.
	TracingProvider *tracing.Provider

	// SSHD is used to execute commands to update or validate OpenSSH config.
	SSHD openssh.SSHD

	// resolver is used to identify the reverse tunnel address when connecting via
	// the proxy.
	resolver reversetunnelclient.Resolver

	// metricRegistry is the prometheus metric registry for the process.
	// Every teleport service that wants to register metrics should use this
	// instead of the global prometheus.DefaultRegisterer to avoid registration
	// conflicts.
	//
	// Both the metricsRegistry and the default global registry are gathered by
	// Telepeort's metric service.
	metricsRegistry *prometheus.Registry

	// state is the process state machine tracking if the process is healthy or not.
	state *processState
}

// processIndex is an internal process index
// to help differentiate between two different teleport processes
// during in-process reload.
var processID int32

func nextProcessID() int32 {
	return atomic.AddInt32(&processID, 1)
}

// GetAuthServer returns the process' auth server
func (process *TeleportProcess) GetAuthServer() *auth.Server {
	return process.localAuth
}

// GetAuditLog returns the process' audit log
func (process *TeleportProcess) GetAuditLog() events.AuditLogSessionStreamer {
	return process.auditLog
}

// GetBackend returns the process' backend
func (process *TeleportProcess) GetBackend() backend.Backend {
	return process.backend
}

// OnHeartbeat generates the default OnHeartbeat callback for the specified component.
func (process *TeleportProcess) OnHeartbeat(component string) func(err error) {
	return func(err error) {
		if err != nil {
			process.BroadcastEvent(Event{Name: TeleportDegradedEvent, Payload: component})
		} else {
			process.BroadcastEvent(Event{Name: TeleportOKEvent, Payload: component})
		}
	}
}

func (process *TeleportProcess) findStaticIdentity(id state.IdentityID) (*state.Identity, error) {
	for i := range process.Config.Identities {
		identity := process.Config.Identities[i]
		if identity.ID.Equals(id) {
			return identity, nil
		}
	}
	return nil, trace.NotFound("identity %v not found", &id)
}

// getConnectors returns a copy of the identities registered for auth server
func (process *TeleportProcess) getConnectors() []*Connector {
	process.Lock()
	defer process.Unlock()

	out := make([]*Connector, 0, len(process.connectors))
	for role := range process.connectors {
		out = append(out, process.connectors[role])
	}
	return out
}

// getInstanceRoles returns the list of enabled service roles.  this differs from simply
// checking the roles of the existing connectors  in two key ways.  First, pseudo-services
// like "admin" or "instance" are not included. Secondly, instance roles are recorded synchronously
// at the time the associated component's init function runs, as opposed to connectors which are
// initialized asynchronously in the background.
func (process *TeleportProcess) getInstanceRoles() []types.SystemRole {
	process.Lock()
	defer process.Unlock()

	out := make([]types.SystemRole, 0, len(process.instanceRoles))
	for role := range process.instanceRoles {
		out = append(out, role)
	}
	return out
}

// getInstanceRoleEventMapping returns the same instance roles as getInstanceRoles, but as a mapping
// of the form `role => event_name`. This can be used to determine what identity event should be
// awaited in order to get a connector for a given role. Used in assertion-based migration to
// iteratively create a system role assertion through each client.
func (process *TeleportProcess) getInstanceRoleEventMapping() map[types.SystemRole]string {
	process.Lock()
	defer process.Unlock()

	out := make(map[types.SystemRole]string, len(process.instanceRoles))
	maps.Copy(out, process.instanceRoles)
	return out
}

// SetExpectedInstanceRole marks a given instance role as active, storing the name of its associated
// identity event.
func (process *TeleportProcess) SetExpectedInstanceRole(role types.SystemRole, eventName string) {
	process.Lock()
	defer process.Unlock()
	process.instanceRoles[role] = eventName
}

// SetExpectedHostedPluginRole marks a given hosted plugin role as active, storing the name of its associated
// identity event.
func (process *TeleportProcess) SetExpectedHostedPluginRole(role types.SystemRole, eventName string) {
	process.Lock()
	defer process.Unlock()
	process.hostedPluginRoles[role] = eventName
}

func (process *TeleportProcess) instanceRoleExpected(role types.SystemRole) bool {
	process.Lock()
	defer process.Unlock()
	_, ok := process.instanceRoles[role]
	return ok
}

func (process *TeleportProcess) hostedPluginRoleExpected(role types.SystemRole) bool {
	process.Lock()
	defer process.Unlock()
	_, ok := process.hostedPluginRoles[role]
	return ok
}

// addConnector adds connector to registered connectors list,
// it will overwrite the connector for the same role
func (process *TeleportProcess) addConnector(connector *Connector) {
	process.Lock()
	defer process.Unlock()

	process.connectors[connector.Role()] = connector
}

// WaitForConnector is a utility function to wait for an identity event and cast
// the resulting payload as a *Connector. Returns (nil, nil) when the
// ExitContext is done, so error checking should happen on the connector rather
// than the error:
//
//	conn, err := process.WaitForConnector("FooIdentity", log)
//	if conn == nil {
//		return trace.Wrap(err)
//	}
func (process *TeleportProcess) WaitForConnector(identityEvent string, log *slog.Logger) (*Connector, error) {
	event, err := process.WaitForEvent(process.ExitContext(), identityEvent)
	if err != nil {
		if log != nil {
			log.DebugContext(process.ExitContext(), "Process is exiting.")
		}
		return nil, nil
	}
	if log != nil {
		log.DebugContext(process.ExitContext(), "Received event.", "event", event.Name)
	}

	conn, ok := (event.Payload).(*Connector)
	if !ok {
		return nil, trace.BadParameter("unsupported connector type: %T", event.Payload)
	}

	return conn, nil
}

// GetID returns the process ID.
func (process *TeleportProcess) GetID() string {
	return process.id
}

func (process *TeleportProcess) setClusterFeatures(features *proto.Features) {
	process.Lock()
	defer process.Unlock()

	if features != nil {
		process.clusterFeatures = *features
	}
}

// GetClusterFeatures returns the cluster features.
func (process *TeleportProcess) GetClusterFeatures() proto.Features {
	process.Lock()
	defer process.Unlock()

	return process.clusterFeatures
}

// setAuthSubjectiveAddr records the peer address that the auth server observed
// for this process during the most recent ping.
func (process *TeleportProcess) setAuthSubjectiveAddr(ip string) {
	process.Lock()
	defer process.Unlock()
	if ip != "" {
		process.authSubjectiveAddr = ip
	}
}

// getAuthSubjectiveAddr accesses the peer address reported by the auth server
// during the most recent ping. May be empty.
func (process *TeleportProcess) getAuthSubjectiveAddr() string {
	process.Lock()
	defer process.Unlock()
	return process.authSubjectiveAddr
}

// getIdentity returns the current identity (credentials to the auth server) for
// a given system role.
func (process *TeleportProcess) getIdentity(role types.SystemRole) (i *state.Identity, err error) {
	process.Lock()
	defer process.Unlock()

	i, err = process.storage.ReadIdentity(state.IdentityCurrent, role)

	if err == nil {
		return i, nil
	}
	if !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	id := state.IdentityID{
		Role:     role,
		HostUUID: process.Config.HostUUID,
		NodeName: process.Config.Hostname,
	}
	if role == types.RoleAdmin {
		// for admin identity use local auth server
		// because admin identity is requested by auth server
		// itself
		principals, dnsNames, err := process.getAdditionalPrincipals(role)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		i, err = auth.GenerateIdentity(process.localAuth, id, principals, dnsNames)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return i, nil
	}

	// try to locate static identity provided in the file
	i, err = process.findStaticIdentity(id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.logger.InfoContext(process.ExitContext(), "Found static identity in the config file, writing to disk.", "identity", logutils.StringerAttr(&id))
	if err = process.storage.WriteIdentity(state.IdentityCurrent, *i); err != nil {
		return nil, trace.Wrap(err)
	}

	return i, nil
}

// Process is a interface for processes
type Process interface {
	// Closer closes all resources used by the process
	io.Closer
	// Start starts the process in a non-blocking way
	Start() error
	// WaitForSignals waits for and handles system process signals.
	WaitForSignals(context.Context, <-chan os.Signal) error
	// ExportFileDescriptors exports service listeners
	// file descriptors used by the process.
	ExportFileDescriptors() ([]*servicecfg.FileDescriptor, error)
	// Shutdown starts graceful shutdown of the process,
	// blocks until all resources are freed and go-routines are
	// shut down.
	Shutdown(context.Context)
	// WaitForEvent waits for one event with the specified name (returns the
	// latest such event if at least one has been broadcasted already, ignoring
	// the context). Returns an error if the context is canceled before an event
	// is received.
	WaitForEvent(ctx context.Context, name string) (Event, error)
	// WaitWithContext waits for the service to stop. This is a blocking
	// function.
	WaitWithContext(ctx context.Context)
}

// NewProcess is a function that creates new teleport from config
type NewProcess func(cfg *servicecfg.Config) (Process, error)

func newTeleportProcess(cfg *servicecfg.Config) (Process, error) {
	return NewTeleport(cfg)
}

// Run installs a signal handler for relevant control signals, starts the
// Teleport process and waits for signals to terminate it or trigger a fork. It
// will also close the process if a critical service exits with an error. The
// process will be closed when the context is done.
func Run(ctx context.Context, cfg servicecfg.Config, newTeleport NewProcess) error {
	sigC := make(chan os.Signal, 1024)
	// this should happen before the very first newTeleport, as that's the point
	// where we MUST handle all the relevant OS signals
	signal.Notify(sigC, teleportSignals...)
	defer signal.Stop(sigC)

	return trace.Wrap(RunWithSignalChannel(ctx, cfg, newTeleport, sigC))
}

// RunWithSignalChannel starts the Teleport process and waits for signals to
// terminate it or trigger a fork. It will also close the process if a critical
// service exits with an error. The process will be closed when the context is
// done.
func RunWithSignalChannel(ctx context.Context, cfg servicecfg.Config, newTeleport NewProcess, sigC <-chan os.Signal) error {
	if newTeleport == nil {
		newTeleport = newTeleportProcess
	}
	copyCfg := cfg
	srv, err := newTeleport(&copyCfg)
	if err != nil {
		return trace.Wrap(err, "initialization failed")
	}
	if srv == nil {
		return trace.BadParameter("process has returned nil server")
	}
	if err := srv.Start(); err != nil {
		return trace.Wrap(err, "startup failed")
	}
	return trace.Wrap(srv.WaitForSignals(ctx, sigC))
}

// NewTeleport takes the daemon configuration, instantiates all required services
// and starts them under a supervisor, returning the supervisor object.
func NewTeleport(cfg *servicecfg.Config) (_ *TeleportProcess, err error) {
	// Before we do anything reset the SIGINT handler back to the default.
	system.ResetInterruptSignalHandler()

	// Validate the config before accessing it.
	if err := servicecfg.ValidateConfig(cfg); err != nil {
		return nil, trace.Wrap(err, "configuration error")
	}

	processID := fmt.Sprintf("%v", nextProcessID())
	cfg.Logger = cfg.Logger.With(
		teleport.ComponentKey, teleport.Component(teleport.ComponentProcess, processID),
		"pid", fmt.Sprintf("%v.%v", os.Getpid(), processID),
	)

	defer func() {
		if err != nil {
			cfg.Logger.ErrorContext(context.Background(), "Failed to start Teleport instance", "error", err)
		}
	}()

	// Use the custom metrics registry if specified, else create a new one.
	// We must create the registry in NewTeleport, as opposed to ApplyConfig(),
	// because some tests are running multiple Teleport instances from the same
	// config.
	metricsRegistry := cfg.MetricsRegistry
	if metricsRegistry == nil {
		metricsRegistry = prometheus.NewRegistry()
	}

	// If FIPS mode was requested make sure binary is build against BoringCrypto.
	if cfg.FIPS {
		if !modules.GetModules().IsBoringBinary() {
			return nil, trace.BadParameter("binary not compiled against BoringCrypto, check " +
				"that Enterprise FIPS release was downloaded from " +
				"a Teleport account https://teleport.sh")
		}
	}

	if cfg.Auth.Preference.GetPrivateKeyPolicy().IsHardwareKeyPolicy() {
		if modules.GetModules().BuildType() != modules.BuildEnterprise {
			return nil, trace.AccessDenied("Hardware Key support is only available with an enterprise license")
		}
	}

	// If SELinux support is enabled ensure we have a valid configuration,
	// and ensure SELinux itself is configured correctly
	if cfg.SSH.EnableSELinux {
		if runtime.GOOS != "linux" {
			return nil, trace.Errorf("SELinux is only supported on Linux")
		}

		if !cfg.CheckServicesForSELinux() {
			return nil, trace.Errorf("SELinux is only supported for the SSH service")
		}

		if err := selinux.CheckConfiguration(cfg.SSH.EnsureSELinuxEnforcing, cfg.Logger); err != nil {
			return nil, trace.Wrap(err)
		}
		cfg.Logger.InfoContext(context.Background(), "SELinux support is enabled for SSH service")
	}

	// If PAM is enabled, make sure that Teleport was built with PAM support
	// and the PAM library was found at runtime.
	if cfg.SSH.PAM != nil && cfg.SSH.PAM.Enabled {
		if !pam.BuildHasPAM() {
			const errorMessage = "Unable to start Teleport: PAM was enabled in file configuration but this \n" +
				"Teleport binary was built without PAM support. To continue either download a \n" +
				"Teleport binary build with PAM support from https://goteleport.com/teleport \n" +
				"or disable PAM in file configuration."
			return nil, trace.BadParameter("%s", errorMessage)
		}
		if !pam.SystemHasPAM() {
			const errorMessage = "Unable to start Teleport: PAM was enabled in file configuration but this \n" +
				"system does not have the needed PAM library installed. To continue either \n" +
				"install libpam or disable PAM in file configuration."
			return nil, trace.BadParameter("%s", errorMessage)
		}
	}

	// create the data directory if it's missing
	_, err = os.Stat(cfg.DataDir)
	if os.IsNotExist(err) {
		err := os.MkdirAll(cfg.DataDir, os.ModeDir|0o700)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				cfg.Logger.ErrorContext(context.Background(), "Teleport does not have permission to write to the data directory. Ensure that you are running as a user with appropriate permissions.", "data_dir", cfg.DataDir)
			}
			return nil, trace.ConvertSystemError(err)
		}
	}

	if len(cfg.FileDescriptors) == 0 {
		cfg.FileDescriptors, err = importFileDescriptors(cfg.Logger)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	supervisor := NewSupervisor(processID, cfg.Logger)
	storage, err := storage.NewProcessStorage(supervisor.ExitContext(), filepath.Join(cfg.DataDir, teleport.ComponentProcess))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var kubeBackend kubernetesBackend
	// If running in a Kubernetes Pod we must init the backend storage for `host_uuid` storage/retrieval.
	if kubernetes.InKubeCluster() {
		kubeBackend, err = kubernetes.New()
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	// Load `host_uuid` from different storages. If this process is running in a Kubernetes Cluster,
	// readOrGenerateHostID will try to read the `host_uuid` from the Kubernetes Secret. If the
	// key is empty or if not running in a Kubernetes Cluster, it will read the
	// `host_uuid` from local data directory.
	// If no host id is available, it will generate a new host id and persist it to available storages.
	if err := readOrGenerateHostID(supervisor.ExitContext(), cfg, kubeBackend); err != nil {
		return nil, trace.Wrap(err)
	}

	_, err = uuid.Parse(cfg.HostUUID)
	if err != nil && !aws.IsEC2NodeID(cfg.HostUUID) {
		cfg.Logger.WarnContext(supervisor.ExitContext(), "Host UUID is not a true UUID (not eligible for UUID-based proxying)", "host_uuid", cfg.HostUUID)
	}

	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}

	// full heartbeat announces are on average every 2/3 * 6/7 of the default
	// announce TTL, so we pick a slightly shorter TTL here
	const rotationCacheTTL = apidefaults.ServerAnnounceTTL / 2
	rotationCache, err := utils.NewFnCache(utils.FnCacheConfig{
		TTL:         rotationCacheTTL,
		Clock:       cfg.Clock,
		Context:     supervisor.ExitContext(),
		ReloadOnErr: true,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.PluginRegistry == nil {
		cfg.PluginRegistry = plugin.NewRegistry()
	}

	if cfg.DatabaseREPLRegistry == nil {
		cfg.DatabaseREPLRegistry = dbrepl.NewREPLGetter(map[string]dbrepl.REPLNewFunc{
			defaults.ProtocolPostgres: pgrepl.New,
		})
	}

	var cloudLabels labels.Importer

	// Check if we're on a cloud instance, and if we should override the node's hostname.
	imClient := cfg.InstanceMetadataClient
	if imClient == nil {
		providers := []func(ctx context.Context) (imds.Client, error){
			func(ctx context.Context) (imds.Client, error) {
				clt, err := awsimds.NewInstanceMetadataClient(ctx)
				return clt, trace.Wrap(err)
			},
			func(ctx context.Context) (imds.Client, error) {
				return azure.NewInstanceMetadataClient(), nil
			},
			func(ctx context.Context) (imds.Client, error) {
				instancesClient, err := gcp.NewInstancesClient(ctx)
				if err != nil {
					return nil, trace.Wrap(err)
				}

				clt, err := gcpimds.NewInstanceMetadataClient(instancesClient)
				return clt, trace.Wrap(err)
			},
			func(ctx context.Context) (imds.Client, error) {
				return oracleimds.NewInstanceMetadataClient(), nil
			},
		}

		imClient, err = cloud.DiscoverInstanceMetadata(supervisor.ExitContext(), providers)
		if err == nil {
			cfg.Logger.InfoContext(supervisor.ExitContext(),
				"Found an instance metadata service. Teleport will import labels from this cloud instance.",
				"type", imClient.GetType())
		} else if !trace.IsNotFound(err) {
			cfg.Logger.ErrorContext(supervisor.ExitContext(), "Error looking for cloud instance metadata", "error", err)
			// Keep going. Not being able to fetch labels isn't necessarily an error (e.g. the user doesn't need imported
			// labels and hasn't configured their cloud instance for it).
		}
	}

	if imClient != nil && imClient.GetType() != types.InstanceMetadataTypeDisabled {
		cloudHostname, err := imClient.GetHostname(supervisor.ExitContext())
		if err == nil {
			cloudHostname = strings.ReplaceAll(cloudHostname, " ", "_")
			if utils.IsValidHostname(cloudHostname) {
				cfg.Logger.InfoContext(supervisor.ExitContext(), "Overriding hostname with value from cloud tag TeleportHostname", "hostname", cloudHostname)
				cfg.Hostname = cloudHostname

				// cloudHostname exists but is not a valid hostname.
			} else if cloudHostname != "" {
				cfg.Logger.InfoContext(supervisor.ExitContext(), "Found invalid hostname in cloud tag TeleportHostname", "hostname", cloudHostname)
			}
		} else if !trace.IsNotFound(err) {
			cfg.Logger.ErrorContext(supervisor.ExitContext(), "Error looking for hostname tag", "error", err)
			// Keep going.
		}

		cloudLabels, err = labels.NewCloudImporter(supervisor.ExitContext(), &labels.CloudConfig{
			Client: imClient,
			Clock:  cfg.Clock,
		})
		if err != nil {
			cfg.Logger.ErrorContext(supervisor.ExitContext(), "Cloud labels will not be imported", "error", err)
			// Keep going.
		}
	}

	if cloudLabels != nil {
		cloudLabels.Start(supervisor.ExitContext())
	}

	// if user did not provide auth domain name, use this host's name
	if cfg.Auth.Enabled && cfg.Auth.ClusterName == nil {
		cfg.Auth.ClusterName, err = services.NewClusterNameWithRandomID(types.ClusterNameSpecV2{
			ClusterName: cfg.Hostname,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	process := &TeleportProcess{
		PluginRegistry:         cfg.PluginRegistry,
		Clock:                  cfg.Clock,
		Supervisor:             supervisor,
		Config:                 cfg,
		instanceConnectorReady: make(chan struct{}),
		instanceRoles:          make(map[types.SystemRole]string),
		hostedPluginRoles:      make(map[types.SystemRole]string),
		connectors:             make(map[types.SystemRole]*Connector),
		importedDescriptors:    cfg.FileDescriptors,
		storage:                storage,
		rotationCache:          rotationCache,
		id:                     processID,
		logger:                 cfg.Logger,
		cloudLabels:            cloudLabels,
		TracingProvider:        tracing.NoopProvider(),
		metricsRegistry:        metricsRegistry,
	}

	process.registerExpectedServices(cfg)

	// if user started auth and another service (without providing the auth address for
	// that service, the address of the in-process auth will be used
	if process.Config.Auth.Enabled && len(process.Config.AuthServerAddresses()) == 0 {
		process.Config.SetAuthServerAddress(process.Config.Auth.ListenAddr)
	}

	if len(process.Config.AuthServerAddresses()) != 0 && process.Config.AuthServerAddresses()[0].Port(0) == 0 {
		// port appears undefined, attempt early listener creation so that we can get the real port
		listener, err := process.importOrCreateListener(ListenerAuth, process.Config.Auth.ListenAddr.Addr)
		if err == nil {
			process.Config.SetAuthServerAddress(utils.FromAddr(listener.Addr()))
		}
	}

	var resolverAddr utils.NetAddr
	if cfg.Version == defaults.TeleportConfigVersionV3 && !cfg.ProxyServer.IsEmpty() {
		resolverAddr = cfg.ProxyServer
	} else {
		resolverAddr = cfg.AuthServerAddresses()[0]
	}

	process.resolver, err = reversetunnelclient.CachingResolver(
		process.ExitContext(),
		reversetunnelclient.WebClientResolver(&webclient.Config{
			Context:   process.ExitContext(),
			ProxyAddr: resolverAddr.String(),
			Insecure:  lib.IsInsecureDevMode(),
			Timeout:   process.Config.Testing.ClientTimeout,
		}),
		process.Clock,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	upgraderKind, externalUpgrader, upgraderVersion := process.detectUpgrader()

	getHello := func(ctx context.Context) (*proto.UpstreamInventoryHello, error) {
		instanceRoles := process.getInstanceRoles()
		services := make([]string, 0, len(instanceRoles))
		for _, r := range instanceRoles {
			services = append(services, string(r))
		}
		hello := &proto.UpstreamInventoryHello{
			ServerID:         cfg.HostUUID,
			Version:          teleport.Version,
			Services:         services,
			Hostname:         cfg.Hostname,
			ExternalUpgrader: externalUpgrader,
		}

		if upgraderVersion != nil {
			// The UpstreamInventoryHello message wants versions with the leading "v".
			hello.ExternalUpgraderVersion = "v" + upgraderVersion.String()
		}

		if upgraderKind == types.UpgraderKindTeleportUpdate {
			info, err := autoupdate.ReadHelloUpdaterInfo(supervisor.ExitContext(), cfg.Logger, cfg.HostUUID)
			if err != nil {
				// Failing to detect teleport-update info is not fatal, we continue.
				cfg.Logger.WarnContext(supervisor.ExitContext(), "Error recovering teleport-update status, this might affect automatic update tracking and progress.", "error", err)
				info = &types.UpdaterV2Info{UpdaterStatus: types.UpdaterStatus_UPDATER_STATUS_UNREADABLE}
			}
			hello.UpdaterInfo = info
		}
		return hello, nil
	}

	// note: we must create the inventory handle *after* registerExpectedServices because that function determines
	// the list of services (instance roles) to be included in the heartbeat.
	process.inventoryHandle, err = inventory.NewDownstreamHandle(
		process.makeInventoryControlStreamWhenReady,
		getHello,
		inventory.WithDownstreamClock(process.Clock),
	)
	if err != nil {
		return nil, trace.Wrap(err, "building inventory handle")
	}

	process.inventoryHandle.RegisterPingHandler(func(sender inventory.DownstreamSender, ping *proto.DownstreamInventoryPing) {
		systemClock := process.Clock.Now().UTC()
		process.logger.InfoContext(process.ExitContext(), "Handling incoming inventory ping.",
			"id", ping.GetID(),
			"clock", systemClock)
		err := sender.Send(process.ExitContext(), &proto.UpstreamInventoryPong{
			ID:          ping.GetID(),
			SystemClock: timestamppb.New(systemClock),
		})
		if err != nil {
			process.logger.WarnContext(process.ExitContext(), "Failed to respond to inventory ping.", "id", ping.ID, "error", err)
		}
	})

	// if an external upgrader is defined, we need to set up an appropriate upgrade window exporter.
	if upgraderKind != "" {
		if process.Config.Auth.Enabled || process.Config.Proxy.Enabled {
			process.logger.WarnContext(process.ExitContext(), "Use of external upgraders on control-plane instances is not recommended.")
		}

		switch upgraderKind {
		case types.UpgraderKindTeleportUpdate:
			// Exports are not required for teleport-update
		case types.UpgraderKindSystemdUnit:
			process.RegisterFunc("autoupdates.endpoint.export", func() error {
				conn, err := waitForInstanceConnector(process, process.logger)
				if err != nil {
					return trace.Wrap(err)
				}
				if conn == nil {
					return trace.BadParameter("process exiting and Instance connector never became available")
				}

				resp, err := conn.Client.Ping(process.ExitContext())
				if err != nil {
					return trace.Wrap(err)
				}
				if !resp.GetServerFeatures().GetCloud() {
					return nil
				}

				if err := endpoint.Export(process.ExitContext(), resolverAddr.String()); err != nil {
					process.logger.WarnContext(process.ExitContext(),
						"Failed to export and validate autoupdates endpoint.",
						"addr", resolverAddr.String(),
						"error", err)
					return trace.Wrap(err)
				}
				process.logger.InfoContext(process.ExitContext(), "Exported autoupdates endpoint.", "addr", resolverAddr.String())
				return nil
			})
			if err := process.configureUpgraderExporter(upgraderKind); err != nil {
				return nil, trace.Wrap(err)
			}
		default:
			if err := process.configureUpgraderExporter(upgraderKind); err != nil {
				return nil, trace.Wrap(err)
			}
		}
	}

	serviceStarted := false

	ps, err := process.newProcessStateMachine()
	if err != nil {
		return nil, trace.Wrap(err, "failed to initialize process state machine")
	}
	process.state = ps

	if !cfg.DiagnosticAddr.IsEmpty() {
		if err := process.initDiagnosticService(); err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentDiagnostic), process.logger)
	}

	if cfg.Tracing.Enabled {
		if err := process.initTracingService(); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	if address := os.Getenv("TELEPORT_PYROSCOPE_SERVER_ADDRESS"); address != "" {
		process.initPyroscope(address)
	}

	if err := process.initDebugService(cfg.DebugService.Enabled); err != nil {
		return nil, trace.Wrap(err)
	}

	// Create a process wide key generator that will be shared. This is so the
	// key generator can pre-generate keys and share these across services.
	if cfg.Keygen == nil {
		cfg.Keygen = keygen.New(process.ExitContext())
	}

	// Produce global TeleportReadyEvent
	// when all components have started
	eventMapping := EventMapping{
		Out: TeleportReadyEvent,
		In:  []string{InstanceReady},
	}

	// Register additional ready events before considering the Teleport instance "ready."
	// Meant for enterprise support.
	if cfg.AdditionalReadyEvents != nil {
		eventMapping.In = append(eventMapping.In, cfg.AdditionalReadyEvents...)
	}

	if cfg.Auth.Enabled {
		eventMapping.In = append(eventMapping.In, AuthTLSReady)
	}
	if cfg.SSH.Enabled {
		eventMapping.In = append(eventMapping.In, NodeSSHReady)
	}
	if cfg.Proxy.Enabled {
		eventMapping.In = append(eventMapping.In, ProxySSHReady)
	}
	if cfg.Relay.Enabled {
		eventMapping.In = append(eventMapping.In, RelayReady)
	}
	if cfg.Kube.Enabled {
		eventMapping.In = append(eventMapping.In, KubernetesReady)
	}
	if cfg.Apps.Enabled {
		eventMapping.In = append(eventMapping.In, AppsReady)
	}
	if process.shouldInitDatabases() {
		eventMapping.In = append(eventMapping.In, DatabasesReady)
	}
	if cfg.Metrics.Enabled {
		eventMapping.In = append(eventMapping.In, MetricsReady)
	}
	if cfg.WindowsDesktop.Enabled {
		eventMapping.In = append(eventMapping.In, WindowsDesktopReady)
	}
	if cfg.Tracing.Enabled {
		eventMapping.In = append(eventMapping.In, TracingReady)
	}
	if process.shouldInitDiscovery() {
		eventMapping.In = append(eventMapping.In, DiscoveryReady)
	}

	process.RegisterEventMapping(eventMapping)

	if cfg.Auth.Enabled {
		if err := process.initAuthService(); err != nil {
			return nil, trace.Wrap(err)
		}
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentAuth), process.logger)
	}

	// initInstance initializes the pseudo-service "Instance" that is active for all teleport
	// instances. All other services inherit their auth client from the "Instance" service, so
	// we initialize it immediately after auth in order to ensure timely client availability.
	if err := process.initInstance(); err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.SSH.Enabled {
		if err := process.initSSH(); err != nil {
			return nil, err
		}
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentNode), process.logger)
	}

	if cfg.Proxy.Enabled {
		if err := process.initProxy(); err != nil {
			return nil, err
		}
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentProxy), process.logger)
	}

	if cfg.Relay.Enabled {
		process.initRelay()
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentRelay), process.logger)
	}

	if cfg.Kube.Enabled {
		process.initKubernetes()
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentKube), process.logger)
	}

	// If this process is proxying applications, start application access server.
	if cfg.Apps.Enabled {
		process.initApps()
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentApp), process.logger)
	}

	if process.shouldInitDatabases() {
		process.initDatabases()
		serviceStarted = true
	} else {
		if process.Config.Databases.Enabled {
			process.logger.WarnContext(process.ExitContext(), "Database service is enabled with empty configuration, skipping initialization")
		}
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentDatabase), process.logger)
	}

	if cfg.Metrics.Enabled {
		process.initMetricsService()
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentMetrics), process.logger)
	}

	if cfg.WindowsDesktop.Enabled {
		process.initWindowsDesktopService()
		serviceStarted = true
	} else {
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentWindowsDesktop), process.logger)
	}

	if process.shouldInitDiscovery() {
		process.initDiscovery()
		serviceStarted = true
	} else {
		if process.Config.Discovery.Enabled {
			process.logger.WarnContext(process.ExitContext(), "Discovery service is enabled with empty configuration, skipping initialization")
		}
		warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentDiscovery), process.logger)
	}

	if process.enterpriseServicesEnabledWithCommunityBuild() {
		var services []string
		if process.Config.Okta.Enabled {
			services = append(services, "okta")
		}
		if process.Config.Jamf.Enabled() {
			services = append(services, "jamf")
		}
		return nil, trace.BadParameter("Attempting to use enterprise only services %v, with a community teleport build", services)
	}

	// Enterprise services will be handled by the enterprise binary. We'll let these set serviceStarted
	// to true and let the enterprise binary error if need be.
	if process.enterpriseServicesEnabled() {
		serviceStarted = true
	}

	if cfg.OpenSSH.Enabled {
		process.initOpenSSH()
		serviceStarted = true
	} else {
		process.RegisterFunc("common.rotate", process.periodicSyncRotationState)
	}

	// run one upload completer per-process
	// even in sync recording modes, since the recording mode can be changed
	// at any time with dynamic configuration
	process.RegisterFunc("common.upload.init", process.initUploaderService)

	if !serviceStarted {
		return nil, trace.BadParameter("all services failed to start")
	}

	// create the new pid file only after started successfully
	if cfg.PIDFile != "" {
		if err := createLockedPIDFile(cfg.PIDFile); err != nil {
			return nil, trace.Wrap(err, "creating pidfile")
		}
	}

	// notify parent process that this process has started
	go process.notifyParent()

	return process, nil
}

// detectUpgrader returns metadata about auto-upgraders that may be active.
// Note that kind and externalName are usually the same.
// However, some unregistered upgraders like the AWS ODIC upgrader are not valid kinds.
// For these upgraders, kind is empty and externalName is set to a non-kind value.
func (process *TeleportProcess) detectUpgrader() (kind, externalName string, version *semver.Version) {
	// Check if the deprecated teleport-upgrader script is being used.
	kind = os.Getenv(automaticupgrades.EnvUpgrader)
	version = automaticupgrades.GetUpgraderVersion(process.GracefulExitContext())
	if version == nil {
		kind = ""
	}

	// If the installation is managed by teleport-update, it supersedes the teleport-upgrader script.
	ok, err := autoupdate.IsManagedByUpdater()
	if err != nil {
		process.logger.WarnContext(process.ExitContext(), "Failed to determine if auto-updates are enabled.", "error", err)
	} else if ok {
		// If this is a teleport-update managed installation, the version
		// managed by the timer will always match the installed version of teleport.
		kind = types.UpgraderKindTeleportUpdate
		version = teleport.SemVer()
	}

	// Instances deployed using the AWS OIDC integration are automatically updated
	// by the proxy. The instance heartbeat should properly reflect that.
	externalName = kind
	if externalName == "" && os.Getenv(types.InstallMethodAWSOIDCDeployServiceEnvVar) == "true" {
		externalName = types.OriginIntegrationAWSOIDC
	}
	return kind, externalName, version
}

// configureUpgraderExporter configures the window exporter for upgraders that export windows.
func (process *TeleportProcess) configureUpgraderExporter(kind string) error {
	driver, err := uw.NewDriver(kind)
	if err != nil {
		return trace.Wrap(err)
	}

	exporter, err := uw.NewExporter(uw.ExporterConfig[inventory.DownstreamSender]{
		Driver:                   driver,
		ExportFunc:               process.exportUpgradeWindows,
		AuthConnectivitySentinel: process.inventoryHandle.Sender(),
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.RegisterCriticalFunc("upgradeewindow.export", exporter.Run)
	process.OnExit("upgradewindow.export.stop", func(_ any) {
		exporter.Close()
	})

	process.logger.InfoContext(process.ExitContext(), "Configured upgrade window exporter for external upgrader.", "kind", kind)
	return nil
}

// enterpriseServicesEnabled will return true if any enterprise services are enabled.
func (process *TeleportProcess) enterpriseServicesEnabled() bool {
	return modules.GetModules().BuildType() == modules.BuildEnterprise &&
		(process.Config.Okta.Enabled || process.Config.Jamf.Enabled())
}

// enterpriseServicesEnabledWithCommunityBuild will return true if any
// enterprise services are enabled with an OSS teleport build.
func (process *TeleportProcess) enterpriseServicesEnabledWithCommunityBuild() bool {
	return modules.GetModules().IsOSSBuild() &&
		(process.Config.Okta.Enabled || process.Config.Jamf.Enabled())
}

// notifyParent notifies parent process that this process has started
// by writing to in-memory pipe used by communication channel.
func (process *TeleportProcess) notifyParent() {
	signalPipe, err := process.importSignalPipe()
	if err != nil {
		if !trace.IsNotFound(err) {
			process.logger.WarnContext(process.ExitContext(), "Failed to import signal pipe")
		}
		process.logger.DebugContext(process.ExitContext(), "No signal pipe to import, must be first Teleport process.")
		return
	}
	defer signalPipe.Close()

	ctx, cancel := context.WithTimeout(process.ExitContext(), signalPipeTimeout)
	defer cancel()

	if _, err := process.WaitForEvent(ctx, TeleportReadyEvent); err != nil {
		process.logger.ErrorContext(process.ExitContext(), "Timeout waiting for a forked process to start. Initiating self-shutdown.", "error", ctx.Err())
		if err := process.Close(); err != nil {
			process.logger.WarnContext(process.ExitContext(), "Failed to shutdown process.", "error", err)
		}
		return
	}
	process.logger.InfoContext(process.ExitContext(), "New service has started successfully.")

	if err := process.writeToSignalPipe(signalPipe, fmt.Sprintf("Process %v has started.", os.Getpid())); err != nil {
		process.logger.WarnContext(process.ExitContext(), "Failed to write to signal pipe", "error", err)
		// despite the failure, it's ok to proceed,
		// it could mean that the parent process has crashed and the pipe
		// is no longer valid.
	}
}

func (process *TeleportProcess) setLocalAuth(a *auth.Server) {
	process.Lock()
	defer process.Unlock()
	process.localAuth = a
}

func (process *TeleportProcess) getLocalAuth() *auth.Server {
	process.Lock()
	defer process.Unlock()
	return process.localAuth
}

func (process *TeleportProcess) setInstanceConnector(conn *Connector) {
	process.Lock()
	process.instanceConnector = conn
	process.Unlock()
	process.instanceConnectorReadyOnce.Do(func() {
		close(process.instanceConnectorReady)
	})
}

func (process *TeleportProcess) getInstanceConnector() *Connector {
	process.Lock()
	defer process.Unlock()
	return process.instanceConnector
}

// getInstanceClient tries to ge the current instance client without blocking. May return nil if either the
// instance client has yet to be created, or this is an auth-only instance. Auth-only instances cannot use
// the instance client because auth servers need to be able to fully initialize without a valid CA in order
// to support HSMs.
func (process *TeleportProcess) getInstanceClient() *authclient.Client {
	conn := process.getInstanceConnector()
	if conn == nil {
		return nil
	}
	return conn.Client
}

// waitForInstanceConnector waits for the instance connector to become available. returns nil if
// process shutdown is triggered or if this is an auth-only instance. Auth-only instances cannot
// use the instance client because auth servers need to be able to fully initialize without a
// valid CA in order to support HSMs.
func (process *TeleportProcess) waitForInstanceConnector() *Connector {
	select {
	case <-process.instanceConnectorReady:
		return process.getInstanceConnector()
	case <-process.ExitContext().Done():
		return nil
	}
}

// makeInventoryControlStreamWhenReady is the same as makeInventoryControlStream except that it blocks until
// the InstanceReady event is emitted.
func (process *TeleportProcess) makeInventoryControlStreamWhenReady(ctx context.Context) (client.DownstreamInventoryControlStream, error) {
	process.inventorySetupDelay.Do(func() {
		process.WaitForEvent(ctx, InstanceReady)
	})
	return process.makeInventoryControlStream(ctx)
}

func (process *TeleportProcess) makeInventoryControlStream(ctx context.Context) (client.DownstreamInventoryControlStream, error) {
	// if local auth exists, create an in-memory control stream
	if auth := process.getLocalAuth(); auth != nil {
		// we use getAuthSubjectiveAddr to guess our peer address even through we are
		// using an in-memory pipe. this works because heartbeat operations don't start
		// until after their respective services have successfully pinged the auth server.
		return auth.MakeLocalInventoryControlStream(client.ICSPipePeerAddrFn(process.getAuthSubjectiveAddr)), nil
	}

	// fallback to using the instance client
	clt := process.getInstanceClient()
	if clt == nil {
		return nil, trace.Errorf("instance client not yet initialized")
	}
	return clt.InventoryControlStream(ctx)
}

// exportUpgradeWindow is a helper for calling ExportUpgradeWindows either on the local in-memory auth server, or via the instance client, depending on
// which is available.
func (process *TeleportProcess) exportUpgradeWindows(ctx context.Context, req proto.ExportUpgradeWindowsRequest) (proto.ExportUpgradeWindowsResponse, error) {
	if auth := process.getLocalAuth(); auth != nil {
		return auth.ExportUpgradeWindows(ctx, req)
	}

	clt := process.getInstanceClient()
	if clt == nil {
		return proto.ExportUpgradeWindowsResponse{}, trace.Errorf("instance client not yet initialized")
	}
	return clt.ExportUpgradeWindows(ctx, req)
}

// isGroupMember returns whether currently logged user is a member of a group
func isGroupMember(gid int) (bool, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return false, trace.ConvertSystemError(err)
	}
	if slices.Contains(groups, gid) {
		return true, nil
	}
	return false, nil
}

// adminCreds returns admin UID and GID settings based on the OS
func adminCreds() (*int, *int, error) {
	if runtime.GOOS != constants.LinuxOS {
		return nil, nil, nil
	}
	// if the user member of adm linux group,
	// make audit log folder readable by admins
	isAdmin, err := isGroupMember(teleport.LinuxAdminGID)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	if !isAdmin {
		return nil, nil, nil
	}
	uid := os.Getuid()
	gid := teleport.LinuxAdminGID
	return &uid, &gid, nil
}

// initAuthUploadHandler initializes the auth server's upload handler based upon the configuration.
// When configured to store session recordings in external storage, this will be an API client for
// cloud-provider storage. Otherwise a local file-based handler is used which stores the recordings
// on disk.
func initAuthUploadHandler(ctx context.Context, auditConfig types.ClusterAuditConfig, dataDir string, externalAuditStorage *externalauditstorage.Configurator) (events.MultipartHandler, error) {
	uriString := auditConfig.AuditSessionsURI()
	if externalAuditStorage.IsUsed() {
		uriString = externalAuditStorage.GetSpec().SessionRecordingsURI
	}
	if uriString == "" {
		recordsDir := filepath.Join(dataDir, events.RecordsDir)
		if err := os.MkdirAll(recordsDir, teleport.SharedDirMode); err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		handler, err := filesessions.NewHandler(filesessions.Config{
			Directory: recordsDir,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return handler, nil
	}
	uri, err := apiutils.ParseSessionsURI(uriString)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch uri.Scheme {
	case teleport.SchemeGCS:
		config := gcssessions.Config{}
		if err := config.SetFromURL(uri); err != nil {
			return nil, trace.Wrap(err)
		}
		handler, err := gcssessions.DefaultNewHandler(ctx, config)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return handler, nil
	case teleport.SchemeS3:
		config := s3sessions.Config{
			UseFIPSEndpoint: auditConfig.GetUseFIPSEndpoint(),
		}
		if externalAuditStorage.IsUsed() {
			config.CredentialsProvider = externalAuditStorage.CredentialsProvider()
		}
		if err := config.SetFromURL(uri, auditConfig.Region()); err != nil {
			return nil, trace.Wrap(err)
		}

		var handler events.MultipartHandler
		handler, err = s3sessions.NewHandler(ctx, config)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if externalAuditStorage.IsUsed() {
			handler = externalAuditStorage.ErrorCounter.WrapSessionHandler(handler)
		}
		return handler, nil
	case teleport.SchemeAZBlob, teleport.SchemeAZBlobHTTP:
		var config azsessions.Config
		if err := config.SetFromURL(uri); err != nil {
			return nil, trace.Wrap(err)
		}
		handler, err := azsessions.NewHandler(ctx, config)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return handler, nil
	case teleport.SchemeFile:
		if err := os.MkdirAll(uri.Path, teleport.SharedDirMode); err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		handler, err := filesessions.NewHandler(filesessions.Config{
			Directory: uri.Path,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return handler, nil
	default:
		return nil, trace.BadParameter(
			"unsupported scheme for audit_sessions_uri: %q, currently supported schemes are: %v",
			uri.Scheme, strings.Join([]string{
				teleport.SchemeS3, teleport.SchemeGCS, teleport.SchemeAZBlob, teleport.SchemeFile,
			}, ", "))
	}
}

var externalAuditMissingAthenaError = trace.BadParameter("athena audit_events_uri must be configured when External Audit Storage is enabled")

// initAuthExternalAuditLog initializes the auth server's audit log.
func (process *TeleportProcess) initAuthExternalAuditLog(auditConfig types.ClusterAuditConfig, externalAuditStorage *externalauditstorage.Configurator) (events.AuditLogger, error) {
	ctx := process.ExitContext()
	var hasNonFileLog bool
	var loggers []events.AuditLogger
	for _, eventsURI := range auditConfig.AuditEventsURIs() {
		uri, err := apiutils.ParseSessionsURI(eventsURI)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if externalAuditStorage.IsUsed() && (len(loggers) > 0 || uri.Scheme != teleport.ComponentAthena) {
			process.logger.InfoContext(ctx, "Skipping events URI because External Audit Storage is enabled", "events_uri", eventsURI)
			continue
		}
		switch uri.Scheme {
		case pgevents.Schema, pgevents.AltSchema:
			hasNonFileLog = true
			var cfg pgevents.Config
			if err := cfg.SetFromURL(uri); err != nil {
				return nil, trace.Wrap(err)
			}
			logger, err := pgevents.New(ctx, cfg)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			loggers = append(loggers, logger)
		case firestore.GetName():
			hasNonFileLog = true
			cfg := firestoreevents.EventsConfig{}
			err = cfg.SetFromURL(uri)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			logger, err := firestoreevents.New(cfg)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			loggers = append(loggers, logger)
		case dynamo.GetName():
			hasNonFileLog = true

			cfg := dynamoevents.Config{
				Tablename:               uri.Host,
				Region:                  auditConfig.Region(),
				EnableContinuousBackups: auditConfig.EnableContinuousBackups(),
				EnableAutoScaling:       auditConfig.EnableAutoScaling(),
				ReadMinCapacity:         int32(auditConfig.ReadMinCapacity()),
				ReadMaxCapacity:         int32(auditConfig.ReadMaxCapacity()),
				ReadTargetValue:         auditConfig.ReadTargetValue(),
				WriteMinCapacity:        int32(auditConfig.WriteMinCapacity()),
				WriteMaxCapacity:        int32(auditConfig.WriteMaxCapacity()),
				WriteTargetValue:        auditConfig.WriteTargetValue(),
				RetentionPeriod:         auditConfig.RetentionPeriod(),
				UseFIPSEndpoint:         auditConfig.GetUseFIPSEndpoint(),
			}

			err = cfg.SetFromURL(uri)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			logger, err := dynamoevents.New(ctx, cfg)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			loggers = append(loggers, logger)
		case teleport.ComponentAthena:
			hasNonFileLog = true
			cfg := athena.Config{
				Region:  auditConfig.Region(),
				Backend: process.backend,
			}
			if process.TracingProvider != nil {
				cfg.Tracer = process.TracingProvider.Tracer(teleport.ComponentAthena)
			}
			err = cfg.SetFromURL(uri)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if externalAuditStorage.IsUsed() {
				// External Audit Storage uses the topicArn, largeEventsS3, and
				// queueURL from the athena audit_events_uri passed by cloud,
				// and overwrites the remaining fields.
				if err := cfg.UpdateForExternalAuditStorage(ctx, externalAuditStorage); err != nil {
					return nil, trace.Wrap(err)
				}
			}
			var logger events.AuditLogger
			logger, err = athena.New(ctx, cfg)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if externalAuditStorage.IsUsed() {
				logger = externalAuditStorage.ErrorCounter.WrapAuditLogger(logger)
			}
			if cfg.LimiterBurst > 0 {
				// Wrap athena logger with rate limiter on search events.
				logger, err = events.NewSearchEventLimiter(events.SearchEventsLimiterConfig{
					RefillTime:   cfg.LimiterRefillTime,
					RefillAmount: cfg.LimiterRefillAmount,
					Burst:        cfg.LimiterBurst,
					AuditLogger:  logger,
				})
				if err != nil {
					return nil, trace.Wrap(err)
				}
			}
			loggers = append(loggers, logger)
		case teleport.SchemeFile:
			if uri.Path == "" {
				return nil, trace.BadParameter("unsupported audit uri: %q (missing path component)", uri)
			}
			if uri.Host != "" && uri.Host != "localhost" {
				return nil, trace.BadParameter("unsupported audit uri: %q (nonlocal host component: %q)", uri, uri.Host)
			}
			if err := os.MkdirAll(uri.Path, teleport.SharedDirMode); err != nil {
				return nil, trace.ConvertSystemError(err)
			}
			logger, err := events.NewFileLog(events.FileLogConfig{
				Dir: uri.Path,
			})
			if err != nil {
				return nil, trace.Wrap(err)
			}
			loggers = append(loggers, logger)
		case teleport.SchemeStdout:
			logger := events.NewWriterEmitter(utils.NopWriteCloser(os.Stdout))
			loggers = append(loggers, logger)
		default:
			return nil, trace.BadParameter(
				"unsupported scheme for audit_events_uri: %q, currently supported schemes are: %v",
				uri.Scheme, strings.Join([]string{
					teleport.SchemeFile, dynamo.GetName(), firestore.GetName(),
					pgevents.Schema, teleport.ComponentAthena, teleport.SchemeStdout,
				}, ", "))
		}
	}

	if len(loggers) < 1 {
		if externalAuditStorage.IsUsed() {
			return nil, externalAuditMissingAthenaError
		}
		return nil, nil
	}

	if !auditConfig.ShouldUploadSessions() && hasNonFileLog {
		// if audit events are being exported, session recordings should
		// be exported as well.
		return nil, trace.BadParameter("please specify audit_sessions_uri when using external audit backends")
	}

	if len(loggers) > 1 {
		return events.NewMultiLog(loggers...)
	}

	return loggers[0], nil
}

// initAuthService can be called to initialize auth server service
func (process *TeleportProcess) initAuthService() error {
	var err error
	cfg := process.Config
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentAuth, process.id))

	// Initialize the storage back-ends for keys, events and records
	b, err := process.initAuthStorage()
	if err != nil {
		return trace.Wrap(err)
	}
	process.backend = b

	clusterName := cfg.Auth.ClusterName.GetClusterName()
	ident, err := process.storage.ReadIdentity(state.IdentityCurrent, types.RoleAdmin)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	if ident != nil {
		clusterName = ident.ClusterName
	}

	cn, err := services.NewClusterNameWithRandomID(types.ClusterNameSpecV2{
		ClusterName: clusterName,
	})

	clusterConfig := cfg.ClusterConfiguration
	if clusterConfig == nil {
		clusterConfig, err = local.NewClusterConfigurationService(b)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	// create keystore
	keystoreOpts := &keystore.Options{
		HostUUID:             cfg.HostUUID,
		ClusterName:          cn,
		AuthPreferenceGetter: clusterConfig,
		FIPS:                 cfg.FIPS,
	}

	switch {
	case cfg.Auth.KeyStore.PKCS11 != servicecfg.PKCS11Config{}:
		if !modules.GetModules().Features().GetEntitlement(entitlements.HSM).Enabled {
			return trace.Errorf("PKCS11 HSM support requires a license with the HSM feature enabled: %w", auth.ErrRequiresEnterprise)
		}
	case cfg.Auth.KeyStore.GCPKMS != servicecfg.GCPKMSConfig{}:
		if !modules.GetModules().Features().GetEntitlement(entitlements.HSM).Enabled {
			return trace.Errorf("GCP KMS support requires a license with the HSM feature enabled: %w", auth.ErrRequiresEnterprise)
		}
	case cfg.Auth.KeyStore.AWSKMS != nil:
		if !modules.GetModules().Features().GetEntitlement(entitlements.HSM).Enabled {
			return trace.Errorf("AWS KMS support requires a license with the HSM feature enabled: %w", auth.ErrRequiresEnterprise)
		}
	}

	keyStore, err := keystore.NewManager(process.GracefulExitContext(), &cfg.Auth.KeyStore, keystoreOpts)
	if err != nil {
		return trace.Wrap(err)
	}

	localRecordingEncryption, err := local.NewRecordingEncryptionService(b)
	if err != nil {
		return trace.Wrap(err)
	}

	recordingEncryptionManager, err := recordingencryption.NewManager(recordingencryption.ManagerConfig{
		Backend:       localRecordingEncryption,
		Cache:         localRecordingEncryption,
		KeyStore:      keyStore,
		Logger:        logger,
		ClusterConfig: clusterConfig,
		LockConfig: backend.RunWhileLockedConfig{
			LockConfiguration: backend.LockConfiguration{
				Backend:            process.backend,
				TTL:                time.Second * 30,
				LockNameComponents: []string{"recording_encryption"},
			},
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	clusterConfig = recordingEncryptionManager
	var emitter apievents.Emitter
	var streamer events.Streamer
	var uploadHandler events.MultipartHandler
	var externalAuditStorage *externalauditstorage.Configurator
	encryptedIO, err := recordingencryption.NewEncryptedIO(clusterConfig, recordingEncryptionManager)
	if err != nil {
		return trace.Wrap(err)
	}

	sessionSummarizerProvider := summarizer.NewSessionSummarizerProvider()

	// create the audit log, which will be consuming (and recording) all events
	// and recording all sessions.
	if cfg.Auth.NoAudit {
		// this is for teleconsole
		process.auditLog = events.NewDiscardAuditLog()

		const warningMessage = "Warning: Teleport audit and session recording have been " +
			"turned off. This is dangerous, you will not be able to view audit events " +
			"or save and playback recorded sessions."
		process.logger.WarnContext(process.ExitContext(), warningMessage)
		emitter, streamer = events.NewDiscardEmitter(), events.NewDiscardStreamer()
	} else {
		// check if session recording has been disabled. note, we will continue
		// logging audit events, we just won't record sessions.
		if cfg.Auth.SessionRecordingConfig.GetMode() == types.RecordOff {
			const warningMessage = "Warning: Teleport session recording have been turned off. " +
				"This is dangerous, you will not be able to save and playback sessions."
			process.logger.WarnContext(process.ExitContext(), warningMessage)
		}

		if cfg.FIPS {
			cfg.Auth.AuditConfig.SetUseFIPSEndpoint(types.ClusterAuditConfigSpecV2_FIPS_ENABLED)
		}

		externalAuditStorage, err = process.newExternalAuditStorageConfigurator()
		if err != nil {
			return trace.Wrap(err)
		}

		uploadHandler, err = initAuthUploadHandler(
			process.ExitContext(), cfg.Auth.AuditConfig, filepath.Join(cfg.DataDir, teleport.LogsDir), externalAuditStorage)
		if err != nil {
			if !trace.IsNotFound(err) {
				return trace.Wrap(err)
			}
		}
		streamer, err = events.NewProtoStreamer(events.ProtoStreamerConfig{
			Uploader:                  uploadHandler,
			Encrypter:                 encryptedIO,
			SessionSummarizerProvider: sessionSummarizerProvider,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		// initialize external loggers.  may return (nil, nil) if no
		// external loggers have been defined.
		externalLog, err := process.initAuthExternalAuditLog(cfg.Auth.AuditConfig, externalAuditStorage)
		if err != nil {
			if !trace.IsNotFound(err) {
				return trace.Wrap(err)
			}
		}

		auditServiceConfig := events.AuditLogConfig{
			Context:       process.ExitContext(),
			DataDir:       filepath.Join(cfg.DataDir, teleport.LogsDir),
			ServerID:      cfg.HostUUID,
			UploadHandler: uploadHandler,
			ExternalLog:   externalLog,
			Decrypter:     encryptedIO,
		}
		auditServiceConfig.UID, auditServiceConfig.GID, err = adminCreds()
		if err != nil {
			return trace.Wrap(err)
		}
		localLog, err := events.NewAuditLog(auditServiceConfig)
		if err != nil {
			return trace.Wrap(err)
		}
		process.auditLog = localLog
		if externalLog != nil {
			externalEmitter, ok := externalLog.(apievents.Emitter)
			if !ok {
				return trace.BadParameter("expected emitter, but %T does not emit", externalLog)
			}
			emitter = externalEmitter
		} else {
			emitter = localLog
		}
	}

	checkingEmitter, err := events.NewCheckingEmitter(events.CheckingEmitterConfig{
		Inner:       events.NewMultiEmitter(events.NewLoggingEmitter(process.GetClusterFeatures().Cloud), emitter),
		Clock:       process.Clock,
		ClusterName: clusterName,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	traceClt := tracing.NewNoopClient()
	if cfg.Tracing.Enabled {
		traceConf, err := process.Config.Tracing.Config()
		if err != nil {
			return trace.Wrap(err)
		}
		traceConf.Logger = process.logger.With(teleport.ComponentKey, teleport.ComponentTracing)

		clt, err := tracing.NewStartedClient(process.ExitContext(), *traceConf)
		if err != nil {
			return trace.Wrap(err)
		}

		traceClt = clt
	}

	// Environment variable for disabling the check major version upgrade check and overrides
	// latest known version in backend.
	skipVersionCheckFromEnv := os.Getenv("TELEPORT_UNSTABLE_SKIP_VERSION_UPGRADE_CHECK") != ""

	// first, create the AuthServer
	authServer, err := auth.Init(
		process.ExitContext(),
		auth.InitConfig{
			Backend:                     b,
			VersionStorage:              process.storage,
			SkipVersionCheck:            cfg.SkipVersionCheck || skipVersionCheckFromEnv,
			Authority:                   cfg.Keygen,
			ClusterConfiguration:        clusterConfig,
			AutoUpdateService:           cfg.AutoUpdateService,
			ClusterAuditConfig:          cfg.Auth.AuditConfig,
			ClusterNetworkingConfig:     cfg.Auth.NetworkingConfig,
			SessionRecordingConfig:      cfg.Auth.SessionRecordingConfig,
			ClusterName:                 cn,
			AuthServiceName:             cfg.Hostname,
			DataDir:                     cfg.DataDir,
			HostUUID:                    cfg.HostUUID,
			NodeName:                    cfg.Hostname,
			Authorities:                 cfg.Auth.Authorities,
			ApplyOnStartupResources:     cfg.Auth.ApplyOnStartupResources,
			BootstrapResources:          cfg.Auth.BootstrapResources,
			ReverseTunnels:              cfg.ReverseTunnels,
			Trust:                       cfg.Trust,
			Presence:                    cfg.Presence,
			Events:                      cfg.Events,
			Provisioner:                 cfg.Provisioner,
			Identity:                    cfg.Identity,
			Access:                      cfg.Access,
			StaticTokens:                cfg.Auth.StaticTokens,
			Roles:                       cfg.Auth.Roles,
			AuthPreference:              cfg.Auth.Preference,
			OIDCConnectors:              cfg.OIDCConnectors,
			AuditLog:                    process.auditLog,
			CipherSuites:                cfg.CipherSuites,
			KeyStore:                    keyStore,
			KeyStoreConfig:              cfg.Auth.KeyStore,
			Emitter:                     checkingEmitter,
			Streamer:                    events.NewReportingStreamer(streamer, process.Config.Testing.UploadEventsC),
			TraceClient:                 traceClt,
			FIPS:                        cfg.FIPS,
			LoadAllCAs:                  cfg.Auth.LoadAllCAs,
			AccessMonitoringEnabled:     cfg.Auth.IsAccessMonitoringEnabled(),
			Clock:                       cfg.Clock,
			HTTPClientForAWSSTS:         cfg.Auth.HTTPClientForAWSSTS,
			RecordingEncryption:         recordingEncryptionManager,
			MultipartHandler:            uploadHandler,
			Tracer:                      process.TracingProvider.Tracer(teleport.ComponentAuth),
			Logger:                      logger,
			RunWhileLockedRetryInterval: cfg.Testing.RunWhileLockedRetryInterval,
			SessionSummarizerProvider:   sessionSummarizerProvider,
		}, func(as *auth.Server) error {
			if !process.Config.CachePolicy.Enabled {
				return nil
			}

			cache, err := process.newAccessCacheForServices(accesspoint.Config{
				Setup:        cache.ForAuth,
				CacheName:    []string{teleport.ComponentAuth},
				EventsSystem: true,
				Unstarted:    true,
			}, as.Services)
			if err != nil {
				return trace.Wrap(err)
			}
			as.Cache = cache
			recordingEncryptionManager.SetCache(cache)

			return nil
		})
	if err != nil {
		return trace.Wrap(err)
	}

	lockWatcher, err := services.NewLockWatcher(process.ExitContext(), services.LockWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component: teleport.ComponentAuth,
			Logger:    process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentAuth, process.id)),
			Client:    authServer.Services,
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetLockWatcher(lockWatcher)

	if externalAuditStorage.IsUsed() {
		externalAuditStorage.SetGenerateOIDCTokenFn(authServer.GenerateExternalAuditStorageOIDCToken)
	}

	unifiedResourcesCache, err := services.NewUnifiedResourceCache(process.ExitContext(), services.UnifiedResourceCacheConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			QueueSize:    defaults.UnifiedResourcesQueueSize,
			Component:    teleport.ComponentUnifiedResource,
			Logger:       process.logger.With(teleport.ComponentKey, teleport.ComponentUnifiedResource),
			Client:       authServer,
			MaxStaleness: time.Minute,
		},
		ResourceGetter: authServer,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetUnifiedResourcesCache(unifiedResourcesCache)

	accessRequestCache, err := services.NewAccessRequestCache(services.AccessRequestCacheConfig{
		Events: authServer.Services,
		Getter: authServer.Services,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetAccessRequestCache(accessRequestCache)

	userNotificationCache, err := services.NewUserNotificationCache(services.NotificationCacheConfig{
		Events: authServer.Services,
		Getter: authServer.Cache,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetUserNotificationCache(userNotificationCache)

	globalNotificationCache, err := services.NewGlobalNotificationCache(services.NotificationCacheConfig{
		Events: authServer.Services,
		Getter: authServer.Cache,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetGlobalNotificationCache(globalNotificationCache)

	headlessAuthenticationWatcher, err := local.NewHeadlessAuthenticationWatcher(process.ExitContext(), local.HeadlessAuthenticationWatcherConfig{
		Backend: b,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	authServer.SetHeadlessAuthenticationWatcher(headlessAuthenticationWatcher)

	process.setLocalAuth(authServer)

	// The auth server runs its own upload completer, which is necessary in sync recording modes where
	// a node can abandon an upload before it is competed.
	// (In async recording modes, auth only ever sees completed uploads, as the node's upload completer
	// packages up the parts into a single upload before sending to auth)
	disableCompleter, _ := strconv.ParseBool(os.Getenv("TELEPORT_UNSTABLE_DISABLE_AUTH_UPLOAD_COMPLETER"))
	switch {
	case disableCompleter:
		logger.WarnContext(process.ExitContext(), "auth service's upload completer is disabled, abandoned uploads may accumulate in external storage")
	case uploadHandler != nil:
		err = events.StartNewUploadCompleter(process.ExitContext(), events.UploadCompleterConfig{
			Uploader:       uploadHandler,
			Component:      teleport.ComponentAuth,
			ClusterName:    clusterName,
			AuditLog:       process.auditLog,
			SessionTracker: authServer.Services,
			Semaphores:     authServer.Services,
			ServerID:       cfg.HostUUID,
		})
		if err != nil {
			return trace.Wrap(err, "starting upload completer")
		}
	}

	connector, err := process.connectToAuthService(types.RoleAdmin)
	if err != nil {
		return trace.Wrap(err)
	}

	// second, create the API Server: it's actually a collection of API servers,
	// each serving requests for a "role" which is assigned to every connected
	// client based on their certificate (user, server, admin, etc)
	authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
		ClusterName:         clusterName,
		AccessPoint:         authServer,
		ReadOnlyAccessPoint: authServer,
		MFAAuthenticator:    authServer,
		LockWatcher:         lockWatcher,
		Logger:              process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentAuth, process.id)),
		// Auth Server does explicit device authorization.
		// Various Auth APIs must allow access to unauthorized devices, otherwise it
		// is not possible to acquire device-aware certificates in the first place.
		DeviceAuthorization: authz.DeviceAuthorizationOpts{
			DisableGlobalMode: true,
			DisableRoleMode:   true,
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}
	var accessGraphCAData []byte
	if cfg.AccessGraph.Enabled && cfg.AccessGraph.CA != "" {
		accessGraphCAData, err = os.ReadFile(cfg.AccessGraph.CA)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	apiConf := &auth.APIConfig{
		AuthServer:     authServer,
		Authorizer:     authorizer,
		AuditLog:       process.auditLog,
		PluginRegistry: process.PluginRegistry,
		Emitter:        authServer,
		MetadataGetter: uploadHandler,
		AccessGraph: auth.AccessGraphConfig{
			Enabled:  cfg.AccessGraph.Enabled,
			Address:  cfg.AccessGraph.Addr,
			CA:       accessGraphCAData,
			Insecure: cfg.AccessGraph.Insecure,
		},
	}

	// Auth initialization is done (including creation/updating of all singleton
	// configuration resources) so now we can start the cache.
	if c, ok := authServer.Cache.(*cache.Cache); ok {
		if err := c.Start(); err != nil {
			return trace.Wrap(err)
		}
	}

	// Register TLS endpoint of the auth service
	tlsConfig, err := connector.ServerTLSConfig(cfg.CipherSuites)
	if err != nil {
		return trace.Wrap(err)
	}
	listener, err := process.importOrCreateListener(ListenerAuth, cfg.Auth.ListenAddr.Addr)
	if err != nil {
		logger.ErrorContext(process.ExitContext(), "Failed to bind to address, exiting.", "pid", os.Getpid(), "listen_address", cfg.Auth.ListenAddr.Addr, "error", err)
		return trace.Wrap(err)
	}

	// use listener addr instead of cfg.Auth.ListenAddr in order to support
	// binding to a random port (e.g. `127.0.0.1:0`).
	authAddr := listener.Addr().String()

	// clean up unused descriptors passed for proxy, but not used by it
	warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentAuth), logger)

	if cfg.Auth.PROXYProtocolMode == multiplexer.PROXYProtocolOn {
		logger.InfoContext(process.ExitContext(), "Starting Auth service with external PROXY protocol support.")
	}
	if cfg.Auth.PROXYProtocolMode == multiplexer.PROXYProtocolUnspecified {
		const warning = "'proxy_protocol' unspecified. " +
			"Starting Auth service with external PROXY protocol support, " +
			"but IP pinned connection affected by PROXY headers will not be allowed. " +
			"Set 'proxy_protocol: on' in 'auth_service' config if Auth service runs behind L4 load balancer with enabled " +
			"PROXY protocol, or set 'proxy_protocol: off' otherwise"
		logger.WarnContext(process.ExitContext(), warning)
	}

	muxCAGetter := func(ctx context.Context, id types.CertAuthID, loadKeys bool) (types.CertAuthority, error) {
		return authServer.GetCertAuthority(ctx, id, loadKeys)
	}
	// use multiplexer to leverage support for proxy protocol.
	mux, err := multiplexer.New(multiplexer.Config{
		PROXYProtocolMode:   cfg.Auth.PROXYProtocolMode,
		Listener:            listener,
		ID:                  teleport.Component(process.id),
		CertAuthorityGetter: muxCAGetter,
		LocalClusterName:    connector.ClusterName(),
	})
	if err != nil {
		listener.Close()
		return trace.Wrap(err)
	}
	go mux.Serve()
	authMetrics := &auth.Metrics{GRPCServerLatency: cfg.Metrics.GRPCServerLatency}

	tlsServer, err := auth.NewTLSServer(process.ExitContext(), auth.TLSServerConfig{
		TLS:                  tlsConfig,
		GetClientCertificate: connector.ClientGetCertificate,

		APIConfig:     *apiConf,
		LimiterConfig: cfg.Auth.Limiter,
		AccessPoint:   authServer.Cache,
		Component:     teleport.Component(teleport.ComponentAuth, process.id),
		ID:            process.id,
		Listener:      mux.TLS(),
		Metrics:       authMetrics,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	process.RegisterCriticalFunc("auth.tls", func() error {
		logger.InfoContext(process.ExitContext(), "Auth service is starting.", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", authAddr)

		// since tlsServer.Serve is a blocking call, we emit this even right before
		// the service has started
		process.BroadcastEvent(Event{Name: AuthTLSReady, Payload: nil})
		err := tlsServer.Serve()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.WarnContext(process.ExitContext(), "TLS server exited with error.", "error", err)
		}
		return nil
	})
	process.RegisterFunc("auth.heartbeat.broadcast", func() error {
		// External integrations rely on this event:
		process.BroadcastEvent(Event{Name: AuthIdentityEvent, Payload: connector})
		process.OnExit("auth.broadcast", func(payload any) {
			connector.Close()
		})
		return nil
	})

	host, port, err := net.SplitHostPort(authAddr)
	if err != nil {
		return trace.Wrap(err)
	}
	// advertise-ip is explicitly set:
	if process.Config.AdvertiseIP != "" {
		ahost, aport, err := utils.ParseAdvertiseAddr(process.Config.AdvertiseIP)
		if err != nil {
			return trace.Wrap(err)
		}
		// if port is not set in the advertise addr, use the default one
		if aport == "" {
			aport = port
		}
		authAddr = net.JoinHostPort(ahost, aport)
	} else {
		// advertise-ip is not set, while the CA is listening on 0.0.0.0? lets try
		// to guess the 'advertise ip' then:
		if net.ParseIP(host).IsUnspecified() {
			ip, err := utils.GuessHostIP()
			if err != nil {
				logger.WarnContext(process.ExitContext(), "failed guessing the host ip address", "error", err)
			} else {
				authAddr = net.JoinHostPort(ip.String(), port)
			}
		}
		logger.WarnContext(process.ExitContext(), "Configuration setting auth_service/advertise_ip is not set, using inferred address", "address", authAddr)
	}

	heartbeat, err := srv.NewHeartbeat(srv.HeartbeatConfig{
		Mode:      srv.HeartbeatModeAuth,
		Context:   process.GracefulExitContext(),
		Component: teleport.ComponentAuth,
		Announcer: authServer,
		GetServerInfo: func() (types.Resource, error) {
			srv := types.ServerV2{
				Kind:    types.KindAuthServer,
				Version: types.V2,
				Metadata: types.Metadata{
					Namespace: apidefaults.Namespace,
					Name:      process.Config.HostUUID,
				},
				Spec: types.ServerSpecV2{
					Addr:     authAddr,
					Hostname: process.Config.Hostname,
					Version:  teleport.Version,
				},
			}
			state, err := process.storage.GetState(process.GracefulExitContext(), types.RoleAdmin)
			if err != nil {
				if !trace.IsNotFound(err) {
					logger.WarnContext(process.ExitContext(), "Failed to get rotation state.", "error", err)
					return nil, trace.Wrap(err)
				}
			} else {
				srv.Spec.Rotation = state.Spec.Rotation
			}
			srv.SetExpiry(process.Clock.Now().UTC().Add(apidefaults.ServerAnnounceTTL))
			return &srv, nil
		},
		KeepAlivePeriod: apidefaults.ServerKeepAliveTTL(),
		AnnouncePeriod:  apidefaults.ServerAnnounceTTL/2 + utils.RandomDuration(apidefaults.ServerAnnounceTTL/10),
		CheckPeriod:     defaults.HeartbeatCheckPeriod,
		ServerTTL:       apidefaults.ServerAnnounceTTL,
		OnHeartbeat:     process.OnHeartbeat(teleport.ComponentAuth),
	})
	if err != nil {
		return trace.Wrap(err)
	}
	process.RegisterFunc("auth.heartbeat", heartbeat.Run)

	spiffeFedSyncer, err := machineidv1.NewSPIFFEFederationSyncer(machineidv1.SPIFFEFederationSyncerConfig{
		Backend:       b,
		Store:         authServer.Services.SPIFFEFederations,
		EventsWatcher: authServer.Services.Events,
		Clock:         process.Clock,
		Logger: logger.With(
			teleport.ComponentKey, teleport.Component(teleport.ComponentAuth, "spiffe_federation_syncer"),
		),
	})
	if err != nil {
		return trace.Wrap(err, "initializing SPIFFEFederation Syncer")
	}
	process.RegisterFunc("auth.spiffe_federation_syncer", func() error {
		return trace.Wrap(spiffeFedSyncer.Run(process.GracefulExitContext()), "running SPIFFEFederation Syncer")
	})

	agentRolloutController, err := rollout.NewController(authServer, logger, process.Clock, cfg.Auth.AgentRolloutControllerSyncPeriod, process.metricsRegistry)
	if err != nil {
		return trace.Wrap(err, "creating the rollout controller")
	}
	process.RegisterFunc("auth.autoupdate_agent_rollout_controller", func() error {
		return trace.Wrap(agentRolloutController.Run(process.GracefulExitContext()), "running autoupdate_agent_rollout controller")
	})

	process.RegisterFunc("auth.server_info", func() error {
		return trace.Wrap(auth.ReconcileServerInfos(process.GracefulExitContext(), authServer))
	})
	process.RegisterFunc("auth.server.system-clock-monitor", func() error {
		return trace.Wrap(authServer.MonitorSystemTime(process.GracefulExitContext()))
	})

	expiry, err := expiry.New(&expiry.Config{
		Log: logger.With(
			teleport.ComponentKey, teleport.Component(teleport.ComponentAuth, "expiry_service"),
		),
		Emitter:     authServer,
		AccessPoint: authServer.Services,
		Clock:       process.Clock,
		HostID:      cfg.HostUUID,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	process.RegisterFunc("auth.expiry", func() error {
		if err := expiry.Run(process.GracefulExitContext()); err != nil {
			logger.ErrorContext(process.GracefulExitContext(), "expiry starting", "error", err)
		}
		return trace.Wrap(err)
	})

	process.RegisterFunc("auth.recording_encryption_resolver", func() error {
		return trace.Wrap(recordingEncryptionManager.Watch(process.GracefulExitContext(), authServer.Services))
	})

	awsRolesAnywhereProfileSyncProcessName := "aws-roles-anywhere.profile-sync"
	process.RegisterFunc("auth."+awsRolesAnywhereProfileSyncProcessName+".service", func() error {
		ctx := process.GracefulExitContext()
		logger := process.logger.With("process", awsRolesAnywhereProfileSyncProcessName)

		if _, err := process.WaitForEvent(ctx, TeleportReadyEvent); err != nil {
			logger.DebugContext(ctx, "process exiting: failed to start AWS Roles Anywhere profile sync service")
			return nil
		}

		params := awsra.AWSRolesAnywherProfileSyncerParams{
			Clock:             process.Clock,
			Logger:            logger,
			KeyStoreManager:   authServer.GetKeyStore(),
			Cache:             authServer.Cache,
			StatusReporter:    authServer.Services,
			AppServerUpserter: authServer.Services,
			HostUUID:          process.Config.HostUUID,
		}

		runWhileLockedConfig := backend.RunWhileLockedConfig{
			LockConfiguration: backend.LockConfiguration{
				Backend:            process.backend,
				LockNameComponents: []string{awsRolesAnywhereProfileSyncProcessName},
				TTL:                1 * time.Minute,
			},
			RefreshLockInterval: 20 * time.Second,
		}

		runFunction := func(ctx context.Context) error {
			return trace.Wrap(awsra.RunAWSRolesAnywherProfileSyncer(ctx, params))
		}

		waitWithJitter := retryutils.SeventhJitter(time.Second * 10)
		for {
			err := backend.RunWhileLocked(ctx, runWhileLockedConfig, runFunction)
			if err != nil && ctx.Err() == nil {
				logger.ErrorContext(
					ctx,
					"AWS Roles Anywhere profile syncer encountered a fatal error, it will restart after backoff",
					"error", err,
					"restart_after", waitWithJitter,
				)
			}

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(waitWithJitter):
			}
		}
	})

	// execute this when process is asked to exit:
	process.OnExit("auth.shutdown", func(payload any) {
		// The listeners have to be closed here, because if shutdown
		// was called before the start of the http server,
		// the http server would have not started tracking the listeners
		// and http.Shutdown will do nothing.
		if mux != nil {
			warnOnErr(process.ExitContext(), mux.Close(), logger)
		}
		if listener != nil {
			warnOnErr(process.ExitContext(), listener.Close(), logger)
		}
		if payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			warnOnErr(process.ExitContext(), tlsServer.Close(), logger)
		} else {
			ctx := payloadContext(payload)
			logger.InfoContext(ctx, "Shutting down immediately (auth service does not currently support graceful shutdown).")
			// NOTE: Graceful shutdown of auth.TLSServer is disabled right now, because we don't
			// have a good model for performing it.  In particular, watchers and other gRPC streams
			// are a problem.  Even if we distinguish between user-created and server-created streams
			// (as is done with ssh connections), we don't have a way to distinguish "service accounts"
			// such as access workflow plugins from normal users.  Without this, a graceful shutdown
			// of the auth server basically never exits.
			warnOnErr(ctx, tlsServer.Close(), logger)

			if g, ok := authServer.Services.UsageReporter.(usagereporter.GracefulStopper); ok {
				if err := g.GracefulStop(ctx); err != nil {
					logger.WarnContext(ctx, "Error while gracefully stopping usage reporter.", "error", err)
				}
			}
		}
		logger.InfoContext(process.ExitContext(), "Exited.")
	})
	return nil
}

func payloadContext(payload any) context.Context {
	if ctx, ok := payload.(context.Context); ok {
		return ctx
	}

	return context.TODO()
}

// OnExit allows individual services to register a callback function which will be
// called when Teleport Process is asked to exit. Usually services terminate themselves
// when the callback is called
func (process *TeleportProcess) OnExit(serviceName string, callback func(any)) {
	process.RegisterFunc(serviceName, func() error {
		event, _ := process.WaitForEvent(context.TODO(), TeleportExitEvent)
		callback(event.Payload)
		return nil
	})
}

// newAccessCache returns new local cache access point
func (process *TeleportProcess) newAccessCacheForServices(cfg accesspoint.Config, services *auth.Services) (*cache.Cache, error) {
	cfg.Context = process.ExitContext()
	cfg.ProcessID = process.id
	cfg.TracingProvider = process.TracingProvider
	cfg.MaxRetryPeriod = process.Config.CachePolicy.MaxRetryPeriod
	cfg.Registerer = process.metricsRegistry

	cfg.Access = services.Access
	cfg.AccessLists = services.AccessLists
	cfg.AccessMonitoringRules = services.AccessMonitoringRules
	cfg.AppSession = services.Identity
	cfg.Applications = services.Applications
	cfg.ClusterConfig = services.ClusterConfigurationInternal
	cfg.CrownJewels = services.CrownJewels
	cfg.DatabaseObjects = services.DatabaseObjects
	cfg.DatabaseServices = services.DatabaseServices
	cfg.Databases = services.Databases
	cfg.DiscoveryConfigs = services.DiscoveryConfigs
	cfg.DynamicAccess = services.DynamicAccessExt
	cfg.Events = services.Events
	cfg.Integrations = services.Integrations
	cfg.KubeWaitingContainers = services.KubeWaitingContainer
	cfg.Kubernetes = services.Kubernetes
	cfg.Notifications = services.Notifications
	cfg.Okta = services.Okta
	cfg.Presence = services.PresenceInternal
	cfg.Provisioner = services.Provisioner
	cfg.Restrictions = services.Restrictions
	cfg.SAMLIdPServiceProviders = services.SAMLIdPServiceProviders
	cfg.SecReports = services.SecReports
	cfg.SnowflakeSession = services.Identity
	cfg.SPIFFEFederations = services.SPIFFEFederations
	cfg.StaticHostUsers = services.StaticHostUser
	cfg.Trust = services.TrustInternal
	cfg.UserGroups = services.UserGroups
	cfg.UserTasks = services.UserTasks
	cfg.UserLoginStates = services.UserLoginStates
	cfg.Users = services.Identity
	cfg.WebSession = services.Identity.WebSessions()
	cfg.WebToken = services.Identity.WebTokens()
	cfg.WindowsDesktops = services.WindowsDesktops
	cfg.WorkloadIdentity = services.WorkloadIdentities
	cfg.DynamicWindowsDesktops = services.DynamicWindowsDesktops
	cfg.AutoUpdateService = services.AutoUpdateService
	cfg.ProvisioningStates = services.ProvisioningStates
	cfg.IdentityCenter = services.IdentityCenter
	cfg.PluginStaticCredentials = services.PluginStaticCredentials
	cfg.GitServers = services.GitServers
	cfg.HealthCheckConfig = services.HealthCheckConfig
	cfg.BotInstance = services.BotInstance
	cfg.RecordingEncryption = services.RecordingEncryptionManager
	cfg.Plugin = services.Plugins

	return accesspoint.NewCache(cfg)
}

func (process *TeleportProcess) newAccessCacheForClient(cfg accesspoint.Config, client authclient.ClientI) (*cache.Cache, error) {
	cfg.Context = process.ExitContext()
	cfg.ProcessID = process.id
	cfg.TracingProvider = process.TracingProvider
	cfg.MaxRetryPeriod = process.Config.CachePolicy.MaxRetryPeriod

	cfg.Access = client
	cfg.AccessLists = client.AccessListClient()
	cfg.AccessMonitoringRules = client.AccessMonitoringRuleClient()
	cfg.AppSession = client
	cfg.Applications = client
	cfg.ClusterConfig = client
	cfg.CrownJewels = client.CrownJewelServiceClient()
	cfg.DatabaseObjects = client.DatabaseObjectsClient()
	cfg.DatabaseServices = client
	cfg.Databases = client
	cfg.DiscoveryConfigs = client.DiscoveryConfigClient()
	cfg.DynamicAccess = client
	cfg.Events = client
	cfg.Integrations = client
	cfg.UserTasks = client.UserTasksServiceClient()
	cfg.KubeWaitingContainers = client
	cfg.Kubernetes = client
	cfg.Notifications = client
	cfg.Okta = client.OktaClient()
	cfg.Presence = client
	cfg.Provisioner = client
	cfg.Restrictions = client
	cfg.SAMLIdPServiceProviders = client
	cfg.SecReports = client.SecReportsClient()
	cfg.SnowflakeSession = client
	cfg.StaticHostUsers = client.StaticHostUserClient()
	cfg.Trust = client
	cfg.UserGroups = client
	cfg.UserLoginStates = client.UserLoginStateClient()
	cfg.Users = client
	cfg.WebSession = client.WebSessions()
	cfg.WebToken = client.WebTokens()
	cfg.WindowsDesktops = client
	cfg.DynamicWindowsDesktops = client.DynamicDesktopClient()
	cfg.AutoUpdateService = client
	cfg.GitServers = client.GitServerClient()
	cfg.HealthCheckConfig = client

	return accesspoint.NewCache(cfg)
}

// newLocalCacheForNode returns new instance of access point configured for a local proxy.
func (process *TeleportProcess) newLocalCacheForNode(clt authclient.ClientI, cacheName []string) (authclient.NodeAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForNode, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewNodeWrapper(clt, cache), nil
}

// newLocalCacheForKubernetes returns new instance of access point configured for a kubernetes service.
func (process *TeleportProcess) newLocalCacheForKubernetes(clt authclient.ClientI, cacheName []string) (authclient.KubernetesAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForKubernetes, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewKubernetesWrapper(clt, cache), nil
}

// newLocalCacheForDatabase returns new instance of access point configured for a database service.
func (process *TeleportProcess) newLocalCacheForDatabase(clt authclient.ClientI, cacheName []string) (authclient.DatabaseAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForDatabases, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewDatabaseWrapper(clt, cache), nil
}

type eksClustersEnroller interface {
	EnrollEKSClusters(context.Context, *integrationpb.EnrollEKSClustersRequest, ...grpc.CallOption) (*integrationpb.EnrollEKSClustersResponse, error)
}

type discoveryConfigClient interface {
	UpdateDiscoveryConfigStatus(ctx context.Context, name string, status discoveryconfig.Status) (*discoveryconfig.DiscoveryConfig, error)
	services.DiscoveryConfigsGetter
}

// combinedDiscoveryClient is an auth.Client client with other, specific, services added to it.
type combinedDiscoveryClient struct {
	authclient.ClientI
	discoveryConfigClient
	eksClustersEnroller
	services.UserTasks
}

// newLocalCacheForDiscovery returns a new instance of access point for a discovery service.
func (process *TeleportProcess) newLocalCacheForDiscovery(clt authclient.ClientI, cacheName []string) (authclient.DiscoveryAccessPoint, error) {
	client := combinedDiscoveryClient{
		ClientI:               clt,
		discoveryConfigClient: clt.DiscoveryConfigClient(),
		eksClustersEnroller:   clt.IntegrationAWSOIDCClient(),
		UserTasks:             clt.UserTasksServiceClient(),
	}

	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return client, nil
	}
	cache, err := process.NewLocalCache(clt, cache.ForDiscovery, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return authclient.NewDiscoveryWrapper(client, cache), nil
}

// newLocalCacheForProxy returns new instance of access point configured for a local proxy.
func (process *TeleportProcess) newLocalCacheForProxy(clt authclient.ClientI, cacheName []string) (authclient.ProxyAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForProxy, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewProxyWrapper(clt, cache), nil
}

// newLocalCacheForRemoteProxy returns new instance of access point configured for a remote proxy.
func (process *TeleportProcess) newLocalCacheForRemoteProxy(clt authclient.ClientI, cacheName []string) (authclient.RemoteProxyAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForRemoteProxy, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewRemoteProxyWrapper(clt, cache), nil
}

// newLocalCacheForApps returns new instance of access point configured for a remote proxy.
func (process *TeleportProcess) newLocalCacheForApps(clt authclient.ClientI, cacheName []string) (authclient.AppsAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForApps, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewAppsWrapper(clt, cache), nil
}

// newLocalCacheForWindowsDesktop returns new instance of access point configured for a windows desktop service.
func (process *TeleportProcess) newLocalCacheForWindowsDesktop(clt authclient.ClientI, cacheName []string) (authclient.WindowsDesktopAccessPoint, error) {
	// if caching is disabled, return access point
	if !process.Config.CachePolicy.Enabled {
		return clt, nil
	}

	cache, err := process.NewLocalCache(clt, cache.ForWindowsDesktop, cacheName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return authclient.NewWindowsDesktopWrapper(clt, cache), nil
}

// NewLocalCache returns new instance of access point
func (process *TeleportProcess) NewLocalCache(clt authclient.ClientI, setupConfig cache.SetupConfigFn, cacheName []string) (*cache.Cache, error) {
	return process.newAccessCacheForClient(accesspoint.Config{
		Setup:     setupConfig,
		CacheName: cacheName,
	}, clt)
}

// GetRotation returns the process rotation. The result is internally cached for
// a few minutes, so anything that must get the latest possible version should
// use process.storage.GetState directly, instead (writes to the state that this
// process knows about will invalidate the cache, however).
func (process *TeleportProcess) GetRotation(role types.SystemRole) (*types.Rotation, error) {
	rotation, err := utils.FnCacheGet(process.ExitContext(), process.rotationCache, role, func(ctx context.Context) (*types.Rotation, error) {
		state, err := process.storage.GetState(ctx, role)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &state.Spec.Rotation, nil
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return rotation, nil
}

func (process *TeleportProcess) proxyPublicAddr() utils.NetAddr {
	if len(process.Config.Proxy.PublicAddrs) == 0 {
		return utils.NetAddr{}
	}
	return process.Config.Proxy.PublicAddrs[0]
}

// NewAsyncEmitter wraps client and returns emitter that never blocks, logs some events and checks values.
// It is caller's responsibility to call Close on the emitter once done.
func (process *TeleportProcess) NewAsyncEmitter(clt apievents.Emitter) (*events.AsyncEmitter, error) {
	emitter, err := events.NewCheckingEmitter(events.CheckingEmitterConfig{
		Inner: events.NewMultiEmitter(events.NewLoggingEmitter(process.GetClusterFeatures().Cloud), clt),
		Clock: process.Clock,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// asyncEmitter makes sure that sessions do not block
	// in case if connections are slow
	return events.NewAsyncEmitter(events.AsyncEmitterConfig{
		Inner: emitter,
	})
}

// initInstance initializes the pseudo-service "Instance" that is active on all teleport instances.
func (process *TeleportProcess) initInstance() error {
	var hasNonAuthRole bool
	for _, role := range process.getInstanceRoles() {
		if role != types.RoleAuth {
			hasNonAuthRole = true
			break
		}
	}

	if process.Config.Auth.Enabled && !hasNonAuthRole {
		// if we have a local auth server and no other services, we cannot create an instance client without breaking HSM rotation.
		// instance control stream will be created via in-memory pipe, but until this limitation is resolved
		// or a fully in-memory instance client is implemented, we cannot rely on the instance client existing
		// for purposes other than the control stream.
		// TODO(fspmarshall): implement one of the two potential solutions listed above.
		process.setInstanceConnector(nil)
		process.BroadcastEvent(Event{Name: InstanceReady, Payload: nil})
		return nil
	}
	process.RegisterWithAuthServer(types.RoleInstance, InstanceIdentityEvent)

	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentInstance, process.id))

	process.RegisterCriticalFunc("instance.init", func() error {
		conn, err := process.WaitForConnector(InstanceIdentityEvent, logger)
		if conn == nil {
			return trace.Wrap(err)
		}

		process.setInstanceConnector(conn)
		logger.InfoContext(process.ExitContext(), "Successfully registered instance client.")
		process.BroadcastEvent(Event{Name: InstanceReady, Payload: nil})
		return nil
	})

	return nil
}

// initSSH initializes the "node" role, i.e. a simple SSH server connected to the auth server.
func (process *TeleportProcess) initSSH() error {
	process.RegisterWithAuthServer(types.RoleNode, SSHIdentityEvent)

	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentNode, process.id))

	proxyGetter := reversetunnel.NewConnectedProxyGetter()

	process.RegisterCriticalFunc("ssh.node", func() error {
		// restartingOnGracefulShutdown will be set to true before the function
		// exits if the function is exiting because Teleport is gracefully
		// shutting down as a consequence of internally-triggered reloading or
		// being signaled to restart.
		var restartingOnGracefulShutdown bool

		conn, err := process.WaitForConnector(SSHIdentityEvent, logger)
		if conn == nil {
			return trace.Wrap(err)
		}

		defer func() { warnOnErr(process.ExitContext(), conn.Close(), logger) }()

		cfg := process.Config

		limiter, err := limiter.NewLimiter(cfg.SSH.Limiter)
		if err != nil {
			return trace.Wrap(err)
		}

		authClient, err := process.newLocalCacheForNode(conn.Client, []string{teleport.ComponentNode})
		if err != nil {
			return trace.Wrap(err)
		}

		// If session recording is disabled at the cluster level and the node is
		// attempting to enabled enhanced session recording, show an error.
		recConfig, err := authClient.GetSessionRecordingConfig(process.ExitContext())
		if err != nil {
			return trace.Wrap(err)
		}
		if recConfig.GetMode() == types.RecordOff && cfg.SSH.BPF.Enabled {
			return trace.BadParameter("session recording is disabled at the cluster " +
				"level. To enable enhanced session recording, enable session recording at " +
				"the cluster level, then restart Teleport.")
		}

		// If BPF is enabled in file configuration, but the operating system does
		// not support enhanced session recording (like macOS), exit right away.
		if cfg.SSH.BPF.Enabled && !bpf.SystemHasBPF() {
			return trace.BadParameter("operating system does not support enhanced " +
				"session recording, check Teleport documentation for more details on " +
				"supported operating systems, kernels, and configuration")
		}

		// Start BPF programs. This is blocking and if the BPF programs fail to
		// load, the node will not start. If BPF is not enabled, this will simply
		// return a NOP struct that can be used to discard BPF data.
		ebpf, err := bpf.New(cfg.SSH.BPF)
		if err != nil {
			// Check kernel version if the host can run BPF programs.
			return trace.NewAggregate(
				trace.Wrap(bpf.IsHostCompatible()),
				trace.Wrap(err),
			)
		}
		defer func() { warnOnErr(process.ExitContext(), ebpf.Close(restartingOnGracefulShutdown), logger) }()

		// make sure the default namespace is used
		if ns := cfg.SSH.Namespace; ns != "" && ns != apidefaults.Namespace {
			return trace.BadParameter("cannot start with custom namespace %q, custom namespaces are deprecated. "+
				"use builtin namespace %q, or omit the 'namespace' config option.", ns, apidefaults.Namespace)
		}
		namespace := types.ProcessNamespace(cfg.SSH.Namespace)

		if auditd.IsLoginUIDSet() {
			const warn = "Login UID is set, but it shouldn't be. Incorrect login UID breaks session ID when using auditd. " +
				"Please make sure that Teleport runs as a daemon and any parent process doesn't set the login UID."
			logger.WarnContext(process.ExitContext(), warn)
		}

		useLocalListener := cfg.SSH.ForceListen || !conn.UseTunnel()

		// Provide helpful log message if listen_addr or public_addr are not being
		// used (tunnel is used to connect to cluster).
		//
		// If a tunnel is not being used, set the default here (could not be done in
		// file configuration because at that time it's not known if server is
		// joining cluster directly or through a tunnel).
		if !useLocalListener {
			if !cfg.SSH.Addr.IsEmpty() {
				logger.InfoContext(process.ExitContext(), "Connected to cluster over tunnel connection, ignoring listen_addr setting.")
			}
			if len(cfg.SSH.PublicAddrs) > 0 {
				logger.InfoContext(process.ExitContext(), "Connected to cluster over tunnel connection, ignoring public_addr setting.")
			}
		}
		if useLocalListener && cfg.SSH.Addr.IsEmpty() {
			cfg.SSH.Addr = *defaults.SSHServerListenAddr()
		}

		// asyncEmitter makes sure that sessions do not block
		// in case if connections are slow
		asyncEmitter, err := process.NewAsyncEmitter(conn.Client)
		if err != nil {
			return trace.Wrap(err)
		}
		defer func() { warnOnErr(process.ExitContext(), asyncEmitter.Close(), logger) }()

		lockWatcher, err := services.NewLockWatcher(process.ExitContext(), services.LockWatcherConfig{
			ResourceWatcherConfig: services.ResourceWatcherConfig{
				Component: teleport.ComponentNode,
				Logger:    process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentNode, process.id)),
				Client:    conn.Client,
			},
		})
		if err != nil {
			return trace.Wrap(err)
		}

		storagePresence := local.NewPresenceService(process.storage.BackendStorage)

		// read the host UUID:
		serverID, err := hostid.ReadOrCreateFile(cfg.DataDir)
		if err != nil {
			return trace.Wrap(err)
		}

		sessionController, err := srv.NewSessionController(srv.SessionControllerConfig{
			Semaphores:     authClient,
			AccessPoint:    authClient,
			LockEnforcer:   lockWatcher,
			Emitter:        &events.StreamerAndEmitter{Emitter: asyncEmitter, Streamer: conn.Client},
			Component:      teleport.ComponentNode,
			Logger:         process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentNode, process.id, "sessionctrl")),
			TracerProvider: process.TracingProvider,
			ServerID:       serverID,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		s, err := regular.New(
			process.ExitContext(),
			cfg.SSH.Addr,
			cfg.Hostname,
			conn.ServerGetHostSigners,
			authClient,
			cfg.DataDir,
			cfg.AdvertiseIP,
			process.proxyPublicAddr(),
			conn.Client,
			regular.SetLimiter(limiter),
			regular.SetEmitter(&events.StreamerAndEmitter{Emitter: asyncEmitter, Streamer: conn.Client}),
			regular.SetLabels(cfg.SSH.Labels, cfg.SSH.CmdLabels, process.cloudLabels),
			regular.SetNamespace(namespace),
			regular.SetPermitUserEnvironment(cfg.SSH.PermitUserEnvironment),
			regular.SetCiphers(cfg.Ciphers),
			regular.SetKEXAlgorithms(cfg.KEXAlgorithms),
			regular.SetMACAlgorithms(cfg.MACAlgorithms),
			regular.SetPAMConfig(cfg.SSH.PAM),
			regular.SetRotationGetter(process.GetRotation),
			regular.SetUseTunnel(conn.UseTunnel()),
			regular.SetFIPS(cfg.FIPS),
			regular.SetBPF(ebpf),
			regular.SetOnHeartbeat(process.OnHeartbeat(teleport.ComponentNode)),
			regular.SetAllowTCPForwarding(cfg.SSH.AllowTCPForwarding),
			regular.SetLockWatcher(lockWatcher),
			regular.SetX11ForwardingConfig(cfg.SSH.X11),
			regular.SetAllowFileCopying(cfg.SSH.AllowFileCopying),
			regular.SetConnectedProxyGetter(proxyGetter),
			regular.SetCreateHostUser(!cfg.SSH.DisableCreateHostUser),
			regular.SetStoragePresenceService(storagePresence),
			regular.SetInventoryControlHandle(process.inventoryHandle),
			regular.SetTracerProvider(process.TracingProvider),
			regular.SetSessionController(sessionController),
			regular.SetPublicAddrs(cfg.SSH.PublicAddrs),
			regular.SetStableUNIXUsers(conn.Client.StableUNIXUsersClient()),
			regular.SetSELinuxEnabled(cfg.SSH.EnableSELinux),
		)
		if err != nil {
			return trace.Wrap(err)
		}
		defer func() { warnOnErr(process.ExitContext(), s.Close(), logger) }()

		if s.GetCreateHostUser() {
			staticHostUserReconciler, err := srv.NewStaticHostUserHandler(srv.StaticHostUserHandlerConfig{
				Events:         conn.Client,
				StaticHostUser: conn.Client.StaticHostUserClient(),
				Server:         s,
				Users:          s.GetHostUsers(),
				Sudoers:        s.GetHostSudoers(),
			})
			if err != nil {
				return trace.Wrap(err)
			}
			go func() {
				warnOnErr(process.ExitContext(), staticHostUserReconciler.Run(process.ExitContext()), logger)
			}()
		}

		var resumableServer *resumption.SSHServerWrapper
		if os.Getenv("TELEPORT_UNSTABLE_DISABLE_SSH_RESUMPTION") == "" {
			resumableServer = resumption.NewSSHServerWrapper(resumption.SSHServerWrapperConfig{
				SSHServer: s.HandleConnection,
				HostID:    serverID,
				DataDir:   cfg.DataDir,
			})

			go func() {
				if err := resumableServer.HandoverCleanup(process.GracefulExitContext()); err != nil {
					logger.WarnContext(process.ExitContext(), "Failed to clean up handover sockets.", "error", err)
				}
			}()
		}

		var agentPool *reversetunnel.AgentPool
		if useLocalListener {
			listener, err := process.importOrCreateListener(ListenerNodeSSH, cfg.SSH.Addr.Addr)
			if err != nil {
				return trace.Wrap(err)
			}
			// clean up unused descriptors passed for proxy, but not used by it
			warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentNode), logger)

			logger.InfoContext(process.ExitContext(), "SSH Service is starting.", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", cfg.SSH.Addr.Addr, "cache_policy", process.Config.CachePolicy)

			preDetect := resumption.PreDetectFixedSSHVersion(sshutils.SSHVersionPrefix)
			if resumableServer != nil {
				preDetect = resumableServer.PreDetect
			}

			// Use multiplexer to leverage support for signed PROXY protocol headers.
			mux, err := multiplexer.New(multiplexer.Config{
				Context:             process.ExitContext(),
				PROXYProtocolMode:   multiplexer.PROXYProtocolOff,
				Listener:            listener,
				ID:                  teleport.Component(teleport.ComponentNode, process.id),
				CertAuthorityGetter: authClient.GetCertAuthority,
				LocalClusterName:    conn.ClusterName(),
				PreDetect:           preDetect,
			})
			if err != nil {
				return trace.Wrap(err)
			}

			go func() {
				if err := mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
					process.logger.ErrorContext(process.ExitContext(), "node ssh multiplexer terminated unexpectedly", "error", err, "mux_id", mux.ID)
				}
			}()
			defer mux.Close()

			listener, err = limiter.WrapListener(mux.SSH())
			if err != nil {
				return trace.Wrap(err)
			}

			go s.Serve(listener)
		} else {
			// Start the SSH server. This kicks off updating labels and starting the
			// heartbeat.
			if err := s.Start(); err != nil {
				return trace.Wrap(err)
			}
		}

		if conn.UseTunnel() {
			var serverHandler reversetunnel.ServerHandler = s
			if resumableServer != nil {
				serverHandler = resumableServer
			}

			// Create and start an agent pool.
			agentPool, err = reversetunnel.NewAgentPool(
				process.ExitContext(),
				reversetunnel.AgentPoolConfig{
					Component:            teleport.ComponentNode,
					HostUUID:             conn.HostID(),
					Resolver:             conn.TunnelProxyResolver(),
					Client:               conn.Client,
					AccessPoint:          authClient,
					AuthMethods:          conn.ClientAuthMethods(),
					Cluster:              conn.ClusterName(),
					Server:               serverHandler,
					FIPS:                 process.Config.FIPS,
					ConnectedProxyGetter: proxyGetter,
				})
			if err != nil {
				return trace.Wrap(err)
			}

			err = agentPool.Start()
			if err != nil {
				return trace.Wrap(err)
			}
			logger.InfoContext(process.ExitContext(), "Service is starting in tunnel mode.")
		}

		// Broadcast that the node has started.
		process.BroadcastEvent(Event{Name: NodeSSHReady, Payload: nil})

		// Block and wait while the node is running.
		event, err := process.WaitForEvent(process.ExitContext(), TeleportExitEvent)
		if err != nil {
			if process.ExitContext().Err() != nil {
				// doing a very un-graceful exit
				return nil
			}
			return trace.Wrap(err)
		}

		if event.Payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			warnOnErr(process.ExitContext(), s.Close(), logger)
		} else {
			logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
			ctx := payloadContext(event.Payload)
			restartingOnGracefulShutdown = services.IsProcessReloading(ctx) || services.HasProcessForked(ctx)
			warnOnErr(ctx, s.Shutdown(ctx), logger)
		}

		s.Wait()
		agentPool.Stop()
		agentPool.Wait()

		logger.InfoContext(process.ExitContext(), "Exited.")
		return nil
	})

	return nil
}

// RegisterWithAuthServer uses one time provisioning token obtained earlier
// from the server to get a pair of SSH keys signed by Auth server host
// certificate authority
func (process *TeleportProcess) RegisterWithAuthServer(role types.SystemRole, eventName string) {
	serviceName := strings.ToLower(role.String())

	process.RegisterCriticalFunc(fmt.Sprintf("register.%v", serviceName), func() error {
		if role.IsLocalService() && !process.instanceRoleExpected(role) && !process.hostedPluginRoleExpected(role) {
			// if you hit this error, your probably forgot to call SetExpectedInstanceRole inside of
			// the registerExpectedServices function, or forgot to call SetExpectedHostedPluginRole during
			// the hosted plugin init process.
			process.logger.ErrorContext(process.ExitContext(), "Register called for unexpected instance role (this is a bug).", "role", role)
		}

		connector, err := process.reconnectToAuthService(role)
		if err != nil {
			return trace.Wrap(err)
		}

		process.BroadcastEvent(Event{Name: eventName, Payload: connector})
		return nil
	})
}

// waitForInstanceConnector waits for the instance connector to be ready,
// logging a warning if this is taking longer than expected.
func waitForInstanceConnector(process *TeleportProcess, log *slog.Logger) (*Connector, error) {
	type r struct {
		c   *Connector
		err error
	}
	ch := make(chan r, 1)
	go func() {
		conn, err := process.WaitForConnector(InstanceIdentityEvent, log)
		ch <- r{conn, err}
	}()

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	for {
		select {
		case result := <-ch:
			if result.c == nil {
				return nil, trace.Wrap(result.err, "waiting for instance connector")
			}
			return result.c, nil
		case <-t.C:
			log.WarnContext(process.ExitContext(), "The Instance connector is still not available, process-wide services such as session uploading will not function")
		}
	}
}

// initUploaderService starts a file-based uploader that scans the local streaming logs directory
// (data/log/upload/streaming/default/)
func (process *TeleportProcess) initUploaderService() error {
	component := teleport.Component(teleport.ComponentUpload, process.id)

	logger := process.logger.With(teleport.ComponentKey, component)

	var clusterName string

	type procUploader interface {
		events.Streamer
		events.AuditLogSessionStreamer
		services.SessionTrackerService
	}

	// use the local auth server for uploads if auth happens to be
	// running in this process, otherwise wait for the instance client
	var uploaderClient procUploader
	if la := process.getLocalAuth(); la != nil {
		// The auth service's upload completer is initialized separately,
		// so as a special case we can stop early if auth happens to be
		// the only service running in this process.
		if srs := process.getInstanceRoles(); len(srs) == 1 && srs[0] == types.RoleAuth {
			logger.DebugContext(process.ExitContext(), "this process only runs the auth service, no separate upload completer will run")
			return nil
		}

		uploaderClient = la
		cn, err := la.GetClusterName(process.ExitContext())
		if err != nil {
			return trace.Wrap(err, "cannot get cluster name")
		}
		clusterName = cn.GetClusterName()
	} else {
		logger.DebugContext(process.ExitContext(), "auth is not running in-process, waiting for instance connector")
		conn, err := waitForInstanceConnector(process, logger)
		if err != nil {
			return trace.Wrap(err)
		}
		if conn == nil {
			return trace.BadParameter("process exiting and Instance connector never became available")
		}
		uploaderClient = conn.Client
		clusterName = conn.ClusterName()
	}

	logger.InfoContext(process.ExitContext(), "starting upload completer service")

	// create folder for uploads
	uid, gid, err := adminCreds()
	if err != nil {
		return trace.Wrap(err)
	}

	// prepare directories for uploader
	paths := [][]string{
		{process.Config.DataDir, teleport.LogsDir, teleport.ComponentUpload, events.StreamingSessionsDir, apidefaults.Namespace},
		{process.Config.DataDir, teleport.LogsDir, teleport.ComponentUpload, events.CorruptedSessionsDir, apidefaults.Namespace},
	}
	for _, path := range paths {
		for i := 1; i < len(path); i++ {
			dir := filepath.Join(path[:i+1]...)
			logger.InfoContext(process.ExitContext(), "Creating directory.", "directory", dir)
			err := os.Mkdir(dir, 0o755)
			err = trace.ConvertSystemError(err)
			if err != nil && !trace.IsAlreadyExists(err) {
				return trace.Wrap(err)
			}
			if uid != nil && gid != nil {
				logger.InfoContext(process.ExitContext(), "Setting directory owner.", "directory", dir, "uid", *uid, "gid", *gid)
				err := os.Lchown(dir, *uid, *gid)
				if err != nil {
					return trace.ConvertSystemError(err)
				}
			}
		}
	}

	uploadsDir := filepath.Join(paths[0]...)
	corruptedDir := filepath.Join(paths[1]...)

	fileUploader, err := filesessions.NewUploader(filesessions.UploaderConfig{
		Streamer:                   uploaderClient,
		ScanDir:                    uploadsDir,
		CorruptedDir:               corruptedDir,
		EventsC:                    process.Config.Testing.UploadEventsC,
		InitialScanDelay:           15 * time.Second,
		EncryptedRecordingUploader: uploaderClient,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.RegisterFunc("fileuploader.service", func() error {
		err := fileUploader.Serve(process.ExitContext())
		if err != nil {
			logger.ErrorContext(process.ExitContext(), "File uploader server exited with error.", "error", err)
		}

		return nil
	})

	process.OnExit("fileuploader.shutdown", func(payload any) {
		logger.InfoContext(process.ExitContext(), "File uploader is shutting down.")
		fileUploader.Close()
		logger.InfoContext(process.ExitContext(), "File uploader has shut down.")
	})

	// upload completer scans for uploads that have been initiated, but not completed
	// by the client (aborted or crashed) and completes them. It will be closed once
	// the uploader context is closed.
	handler, err := filesessions.NewHandler(filesessions.Config{Directory: uploadsDir})
	if err != nil {
		return trace.Wrap(err)
	}

	uploadCompleter, err := events.NewUploadCompleter(events.UploadCompleterConfig{
		Component:      component,
		Uploader:       handler,
		AuditLog:       uploaderClient,
		SessionTracker: uploaderClient,
		ClusterName:    clusterName,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.RegisterFunc("fileuploadcompleter.service", func() error {
		if err := uploadCompleter.Serve(process.ExitContext()); err != nil {
			logger.ErrorContext(process.ExitContext(), "File uploader server exited with error.", "error", err)
		}
		return nil
	})

	process.OnExit("fileuploadcompleter.shutdown", func(payload any) {
		logger.InfoContext(process.ExitContext(), "File upload completer is shutting down.", "error", err)
		uploadCompleter.Close()
		logger.InfoContext(process.ExitContext(), "File upload completer has shut down.")
	})

	return nil
}

// promHTTPLogAdapter adapts a slog.Logger into a promhttp.Logger.
type promHTTPLogAdapter struct {
	ctx context.Context
	*slog.Logger
}

// Println implements the promhttp.Logger interface.
func (l promHTTPLogAdapter) Println(v ...any) {
	if !l.Handler().Enabled(l.ctx, slog.LevelError) {
		return
	}

	//nolint:sloglint // msg cannot be constant
	l.ErrorContext(l.ctx, fmt.Sprint(v...))
}

// initMetricsService starts the metrics service currently serving metrics for
// prometheus consumption
func (process *TeleportProcess) initMetricsService() error {
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentMetrics, process.id))

	config := diagnosticHandlerConfig{
		enableMetrics: true,
	}
	mux, err := process.newDiagnosticHandler(config, logger)
	if err != nil {
		return trace.Wrap(err)
	}

	listener, err := process.importOrCreateListener(ListenerMetrics, process.Config.Metrics.ListenAddr.Addr)
	if err != nil {
		return trace.Wrap(err)
	}
	warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentMetrics), logger)

	tlsConfig := &tls.Config{}
	if process.Config.Metrics.MTLS {
		for _, pair := range process.Config.Metrics.KeyPairs {
			certificate, err := tls.LoadX509KeyPair(pair.Certificate, pair.PrivateKey)
			if err != nil {
				return trace.Wrap(err, "failed to read keypair: %+v", err)
			}
			tlsConfig.Certificates = append(tlsConfig.Certificates, certificate)
		}

		if len(tlsConfig.Certificates) == 0 {
			return trace.BadParameter("no keypairs were provided for the metrics service with mtls enabled")
		}

		addedCerts := false
		pool := x509.NewCertPool()
		for _, caCertPath := range process.Config.Metrics.CACerts {
			caCert, err := os.ReadFile(caCertPath)
			if err != nil {
				return trace.Wrap(err, "failed to read prometheus CA certificate %+v", caCertPath)
			}

			if !pool.AppendCertsFromPEM(caCert) {
				return trace.BadParameter("failed to parse prometheus CA certificate: %+v", caCertPath)
			}
			addedCerts = true
		}

		if !addedCerts {
			return trace.BadParameter("no prometheus ca certs were provided for the metrics service with mtls enabled")
		}

		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConfig.ClientCAs = pool
		//nolint:staticcheck // Keep BuildNameToCertificate to avoid changes in legacy behavior.
		tlsConfig.BuildNameToCertificate()

		listener = tls.NewListener(listener, tlsConfig)
	}

	server := &http.Server{
		Handler:           mux,
		ReadTimeout:       apidefaults.DefaultIOTimeout,
		ReadHeaderTimeout: defaults.ReadHeadersTimeout,
		WriteTimeout:      apidefaults.DefaultIOTimeout,
		IdleTimeout:       apidefaults.DefaultIdleTimeout,
		TLSConfig:         tlsConfig,
	}

	logger.InfoContext(process.ExitContext(), "Starting metrics service.", "listen_address", process.Config.Metrics.ListenAddr.Addr)

	process.RegisterFunc("metrics.service", func() error {
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			logger.WarnContext(process.ExitContext(), "Metrics server exited with error.", "error", err)
		}
		return nil
	})

	process.OnExit("metrics.shutdown", func(payload any) {
		if payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			warnOnErr(process.ExitContext(), server.Close(), logger)
		} else {
			logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
			ctx := payloadContext(payload)
			warnOnErr(process.ExitContext(), server.Shutdown(ctx), logger)
		}
		logger.InfoContext(process.ExitContext(), "Exited.")
	})

	process.BroadcastEvent(Event{Name: MetricsReady, Payload: nil})
	return nil
}

// newProcessStateMachine creates a state machine tracking the Teleport process
// state. The state machine is then used by the diagnostics or the debug service
// to evaluate the process health.
func (process *TeleportProcess) newProcessStateMachine() (*processState, error) {
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentDiagnosticHealth, process.id))
	// Create a state machine that will process and update the internal state of
	// Teleport based off Events. Use this state machine to return the
	// status from the /readyz endpoint.
	ps, err := newProcessState(process)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	process.RegisterFunc("readyz.monitor", func() error {
		// Start loop to monitor for events that are used to update Teleport state.
		ctx, cancel := context.WithCancel(process.GracefulExitContext())
		defer cancel()

		eventCh := make(chan Event, 1024)
		process.ListenForEvents(ctx, TeleportDegradedEvent, eventCh)
		process.ListenForEvents(ctx, TeleportOKEvent, eventCh)

		for {
			select {
			case e := <-eventCh:
				ps.update(e)
			case <-ctx.Done():
				logger.DebugContext(process.ExitContext(), "Teleport is exiting, returning.")
				return nil
			}
		}
	})
	return ps, nil
}

// initDiagnosticService starts diagnostic service currently serving healthz
// and prometheus endpoints
func (process *TeleportProcess) initDiagnosticService() error {
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentDiagnostic, process.id))

	config := diagnosticHandlerConfig{
		// support legacy metrics collection in the diagnostic service.
		// metrics will otherwise be served by the metrics service if it's enabled
		// in the config.
		enableMetrics:    !process.Config.Metrics.Enabled,
		enableProfiling:  process.Config.Debug,
		enableHealth:     true,
		enableLogLeveler: false,
	}
	mux, err := process.newDiagnosticHandler(config, logger)
	if err != nil {
		return trace.Wrap(err)
	}

	listener, err := process.importOrCreateListener(ListenerDiagnostic, process.Config.DiagnosticAddr.Addr)
	if err != nil {
		return trace.Wrap(err)
	}
	warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentDiagnostic), logger)

	server := &http.Server{
		Handler:           mux,
		ReadTimeout:       apidefaults.DefaultIOTimeout,
		ReadHeaderTimeout: defaults.ReadHeadersTimeout,
		WriteTimeout:      apidefaults.DefaultIOTimeout,
		IdleTimeout:       apidefaults.DefaultIdleTimeout,
	}

	logger.InfoContext(process.ExitContext(), "Starting diagnostic service.", "listen_address", process.Config.DiagnosticAddr.Addr)

	muxListener, err := multiplexer.New(multiplexer.Config{
		Context:                        process.ExitContext(),
		Listener:                       listener,
		PROXYProtocolMode:              multiplexer.PROXYProtocolUnspecified,
		SuppressUnexpectedPROXYWarning: true,
		ID:                             teleport.Component(teleport.ComponentDiagnostic),
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.RegisterFunc("diagnostic.service", func() error {
		listenerHTTP := muxListener.HTTP()
		go func() {
			if err := muxListener.Serve(); err != nil && !utils.IsOKNetworkError(err) {
				process.logger.ErrorContext(process.ExitContext(), "Mux encountered error serving", "error", err, "mux_id", muxListener.ID)
			}
		}()

		if err := server.Serve(listenerHTTP); !errors.Is(err, http.ErrServerClosed) {
			logger.WarnContext(process.ExitContext(), "Diagnostic server exited with error.", "error", err)
		}
		return nil
	})

	process.OnExit("diagnostic.shutdown", func(payload any) {
		warnOnErr(process.ExitContext(), muxListener.Close(), logger)
		if payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			warnOnErr(process.ExitContext(), server.Close(), logger)
		} else {
			logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
			ctx := payloadContext(payload)
			warnOnErr(process.ExitContext(), server.Shutdown(ctx), logger)
		}
		logger.InfoContext(process.ExitContext(), "Exited.")
	})

	return nil
}

// initDebugService starts debug service serving endpoints used for
// troubleshooting the instance. This service is always active, users can
// disable its sensitive pprof and log-setting endpoints, but the liveness
// and readiness ones are always active.
func (process *TeleportProcess) initDebugService(exposeDebugRoutes bool) error {
	if process.state == nil {
		return trace.BadParameter("teleport process state machine has not yet been initialized (this is a bug)")
	}

	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentDebug, process.id))

	// Unix socket creation can fail on paths too long. Depending on the UNIX implementation,
	// socket paths cannot exceed 104 or 108 chars.
	listener, err := process.importOrCreateListener(ListenerDebug, filepath.Join(process.Config.DataDir, teleport.DebugServiceSocketName))
	if err != nil {
		if exposeDebugRoutes {
			// If the debug service is enabled in the config, this is a hard failure
			return trace.Wrap(err)
		} else {
			// If the debug service was disabled in the config, we issue a warning and will have to continue.
			logger.WarnContext(process.ExitContext(),
				"Failed to open the debug socket. teleport-update will not be able to accurately check Teleport health.",
				"error", err,
			)
			return nil
		}
	}

	// Users can disable the debug service for compliance reasons but not the health
	// routes because the updater relies on them.
	config := diagnosticHandlerConfig{
		enableMetrics:    exposeDebugRoutes,
		enableProfiling:  exposeDebugRoutes,
		enableHealth:     true,
		enableLogLeveler: exposeDebugRoutes,
	}
	mux, err := process.newDiagnosticHandler(config, logger)
	if err != nil {
		return trace.Wrap(err)
	}

	server := &http.Server{
		Handler:           mux,
		ReadTimeout:       apidefaults.DefaultIOTimeout,
		ReadHeaderTimeout: defaults.ReadHeadersTimeout,
		// pprof endpoints support delta profiles and cpu and trace profiling
		// over time, both of which can be effectively unbounded in time; care
		// should be taken when adding more endpoints to this server, however,
		// and if necessary, a timeout can be either added to some more
		// sensitive endpoint, or the timeout can be removed from the more lax
		// ones
		WriteTimeout: 0,
		IdleTimeout:  apidefaults.DefaultIdleTimeout,
	}

	process.RegisterFunc("debug.service", func() error {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.WarnContext(process.ExitContext(), "Debug server exited with error.", "error", err)
		}
		return nil
	})
	warnOnErr(process.ExitContext(), process.closeImportedDescriptors(teleport.ComponentDebug), logger)

	process.OnExit("debug.shutdown", func(payload any) {
		if payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			warnOnErr(process.ExitContext(), server.Close(), logger)
		} else {
			logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
			ctx := payloadContext(payload)
			warnOnErr(process.ExitContext(), server.Shutdown(ctx), logger)
		}
		logger.InfoContext(process.ExitContext(), "Exited.")
	})

	return nil
}

func (process *TeleportProcess) initTracingService() error {
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentTracing, process.id))
	logger.InfoContext(process.ExitContext(), "Initializing tracing provider and exporter.")

	attrs := []attribute.KeyValue{
		attribute.String(tracing.ProcessIDKey, process.id),
		attribute.String(tracing.HostnameKey, process.Config.Hostname),
		attribute.String(tracing.HostIDKey, process.Config.HostUUID),
	}

	traceConf, err := process.Config.Tracing.Config(attrs...)
	if err != nil {
		return trace.Wrap(err)
	}
	traceConf.Logger = process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentTracing, process.id))

	provider, err := tracing.NewTraceProvider(process.ExitContext(), *traceConf)
	if err != nil {
		return trace.Wrap(err)
	}
	process.TracingProvider = provider

	process.OnExit("tracing.shutdown", func(payload any) {
		if payload == nil {
			logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			warnOnErr(process.ExitContext(), provider.Shutdown(ctx), logger)
		} else {
			logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
			ctx := payloadContext(payload)
			warnOnErr(process.ExitContext(), provider.Shutdown(ctx), logger)
		}
		process.logger.InfoContext(process.ExitContext(), "Exited.")
	})

	process.BroadcastEvent(Event{Name: TracingReady, Payload: nil})
	return nil
}

// getAdditionalPrincipals returns a list of additional principals to add
// to role's service certificates.
func (process *TeleportProcess) getAdditionalPrincipals(role types.SystemRole) ([]string, []string, error) {
	var principals []string
	var dnsNames []string
	if process.Config.Hostname != "" {
		principals = append(principals, process.Config.Hostname)
		if lh := utils.ToLowerCaseASCII(process.Config.Hostname); lh != process.Config.Hostname {
			// openssh expects all hostnames to be lowercase
			principals = append(principals, lh)
		}
	}
	var addrs []utils.NetAddr

	// Add default DNSNames to the dnsNames list.
	// For identities generated by teleport <= v6.1.6 the teleport.cluster.local DNS is not present
	dnsNames = append(dnsNames, auth.DefaultDNSNamesForRole(role)...)

	switch role {
	case types.RoleProxy:
		addrs = append(process.Config.Proxy.PublicAddrs,
			process.Config.Proxy.WebAddr,
			process.Config.Proxy.SSHAddr,
			process.Config.Proxy.ReverseTunnelListenAddr,
			process.Config.Proxy.MySQLAddr,
			process.Config.Proxy.PeerAddress,
			utils.NetAddr{Addr: string(teleport.PrincipalLocalhost)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV4)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV6)},
			utils.NetAddr{Addr: reversetunnelclient.LocalKubernetes},
		)
		addrs = append(addrs, process.Config.Proxy.SSHPublicAddrs...)
		addrs = append(addrs, process.Config.Proxy.TunnelPublicAddrs...)
		addrs = append(addrs, process.Config.Proxy.PostgresPublicAddrs...)
		addrs = append(addrs, process.Config.Proxy.MySQLPublicAddrs...)
		addrs = append(addrs, process.Config.Proxy.Kube.PublicAddrs...)
		addrs = append(addrs, process.Config.Proxy.PeerPublicAddr)
		// Automatically add wildcards for every proxy public address for k8s SNI routing
		if process.Config.Proxy.Kube.Enabled {
			for _, publicAddr := range utils.JoinAddrSlices(process.Config.Proxy.PublicAddrs, process.Config.Proxy.Kube.PublicAddrs) {
				host, err := utils.Host(publicAddr.Addr)
				if err != nil {
					return nil, nil, trace.Wrap(err)
				}
				if ip := net.ParseIP(host); ip == nil {
					dnsNames = append(dnsNames, "*."+host)
				}
			}
		}
	case types.RoleRelay:
		dnsNames = append(dnsNames, process.Config.Relay.APIPublicHostnames...)
	case types.RoleAuth, types.RoleAdmin:
		addrs = process.Config.Auth.PublicAddrs
	case types.RoleNode:
		// DELETE IN 5.0: We are manually adding HostUUID here in order
		// to allow UUID based routing to function with older Auth Servers
		// which don't automatically add UUID to the principal list.
		principals = append(principals, process.Config.HostUUID)
		addrs = process.Config.SSH.PublicAddrs
		// If advertise IP is set, add it to the list of principals. Otherwise
		// add in the default (0.0.0.0) which will be replaced by the Auth Server
		// when a host certificate is issued.
		if process.Config.AdvertiseIP != "" {
			advertiseIP, err := utils.ParseAddr(process.Config.AdvertiseIP)
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
			addrs = append(addrs, *advertiseIP)
		} else {
			addrs = append(addrs, process.Config.SSH.Addr)
		}
	case types.RoleKube:
		addrs = append(addrs,
			utils.NetAddr{Addr: string(teleport.PrincipalLocalhost)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV4)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV6)},
			utils.NetAddr{Addr: reversetunnelclient.LocalKubernetes},
		)
		addrs = append(addrs, process.Config.Kube.PublicAddrs...)
	case types.RoleApp, types.RoleOkta:
		principals = append(principals, process.Config.HostUUID)
	case types.RoleWindowsDesktop:
		addrs = append(addrs,
			utils.NetAddr{Addr: string(teleport.PrincipalLocalhost)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV4)},
			utils.NetAddr{Addr: string(teleport.PrincipalLoopbackV6)},
			utils.NetAddr{Addr: reversetunnelclient.LocalWindowsDesktop},
			utils.NetAddr{Addr: desktop.WildcardServiceDNS},
		)
		addrs = append(addrs, process.Config.WindowsDesktop.PublicAddrs...)
	}

	if process.Config.OpenSSH.Enabled {
		for _, a := range process.Config.OpenSSH.AdditionalPrincipals {
			addr, err := utils.ParseAddr(a)
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
			addrs = append(addrs, *addr)
		}
	}

	for _, addr := range addrs {
		if addr.IsEmpty() {
			continue
		}
		host := addr.Host()
		if host == "" {
			host = defaults.BindIP
		}
		principals = append(principals, host)
	}
	return principals, dnsNames, nil
}

// initProxy gets called if teleport runs with 'proxy' role enabled.
// this means it will do several things:
//  1. serve a web UI
//  2. proxy SSH connections to nodes running with 'node' role
//  3. take care of reverse tunnels
//  4. optionally proxy kubernetes connections
//  5. optionally check for automatic upgrades for deployments created by AWS OIDC integrations
func (process *TeleportProcess) initProxy() error {
	// If no TLS key was provided for the web listener, generate a self-signed cert
	if len(process.Config.Proxy.KeyPairs) == 0 &&
		!process.Config.Proxy.DisableTLS &&
		!process.Config.Proxy.ACME.Enabled {
		err := initSelfSignedHTTPSCert(process.Config)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	process.RegisterWithAuthServer(types.RoleProxy, ProxyIdentityEvent)
	process.RegisterCriticalFunc("proxy.init", func() error {
		conn, err := process.WaitForConnector(ProxyIdentityEvent, process.logger)
		if conn == nil {
			return trace.Wrap(err)
		}

		if err := process.initProxyEndpoint(conn); err != nil {
			warnOnErr(process.ExitContext(), conn.Close(), process.logger)
			return trace.Wrap(err)
		}

		return nil
	})
	process.RegisterFunc("update.aws-oidc.deploy.service", func() error {
		err := process.initAWSOIDCDeployServiceUpdater(process.Config.Proxy.AutomaticUpgradesChannels)
		return trace.Wrap(err)
	})
	return nil
}

type proxyListeners struct {
	mux    *multiplexer.Mux
	sshMux *multiplexer.Mux
	tls    *multiplexer.WebListener
	// ssh receives SSH traffic that is multiplexed on the Proxy SSH Port. When TLS routing
	// is enabled only traffic with the TLS ALPN protocol common.ProtocolProxySSH is received.
	ssh net.Listener
	// sshGRPC receives gRPC traffic that is multiplexed on the Proxy SSH Port. When TLS routing
	// is enabled only traffic with the TLS ALPN protocol common.ProtocolProxySSHGRPC is received.
	sshGRPC       net.Listener
	web           net.Listener
	reverseTunnel net.Listener
	kube          net.Listener
	db            dbListeners
	alpn          net.Listener
	// reverseTunnelALPN handles ALPN traffic on the reverse tunnel port when TLS routing
	// is not enabled. It's used to redirect traffic on that port to the gRPC
	// listener.
	reverseTunnelALPN net.Listener
	proxyPeer         net.Listener
	// grpcPublic receives gRPC traffic that has the TLS ALPN protocol common.ProtocolProxyGRPCInsecure. This
	// listener does not enforce mTLS authentication since it's used to handle cluster join requests.
	grpcPublic net.Listener
	// grpcMTLS receives gRPC traffic that has the TLS ALPN protocol common.ProtocolProxyGRPCSecure. This
	// listener is only enabled when TLS routing is enabled and the gRPC server will enforce mTLS authentication.
	grpcMTLS         net.Listener
	reverseTunnelMux *multiplexer.Mux
	// minimalWeb handles traffic on the reverse tunnel port when TLS routing
	// is not enabled. It serves only the subset of web traffic required for
	// agents to join the cluster.
	minimalWeb net.Listener
	minimalTLS *multiplexer.WebListener
}

// Close closes all proxy listeners.
func (l *proxyListeners) Close() {
	if l.mux != nil {
		l.mux.Close()
	}
	if l.sshMux != nil {
		l.sshMux.Close()
	}
	if l.tls != nil {
		l.tls.Close()
	}
	if l.ssh != nil {
		l.ssh.Close()
	}
	if l.sshGRPC != nil {
		l.sshGRPC.Close()
	}
	if l.web != nil {
		l.web.Close()
	}
	if l.reverseTunnel != nil {
		l.reverseTunnel.Close()
	}
	if l.kube != nil {
		l.kube.Close()
	}
	l.db.Close()
	if l.alpn != nil {
		l.alpn.Close()
	}
	if l.reverseTunnelALPN != nil {
		l.reverseTunnelALPN.Close()
	}
	if l.proxyPeer != nil {
		l.proxyPeer.Close()
	}
	if l.grpcPublic != nil {
		l.grpcPublic.Close()
	}
	if l.grpcMTLS != nil {
		l.grpcMTLS.Close()
	}
	if l.reverseTunnelMux != nil {
		l.reverseTunnelMux.Close()
	}
	if l.minimalWeb != nil {
		l.minimalWeb.Close()
	}
	if l.minimalTLS != nil {
		l.minimalTLS.Close()
	}
}

// dbListeners groups database access listeners.
type dbListeners struct {
	// postgres serves Postgres clients.
	postgres net.Listener
	// mysql serves MySQL clients.
	mysql net.Listener
	// mongo serves Mongo clients.
	mongo net.Listener
	// tls serves database clients that use plain TLS handshake.
	tls net.Listener
}

// Empty returns true if no database access listeners are initialized.
func (l *dbListeners) Empty() bool {
	return l.postgres == nil && l.mysql == nil && l.tls == nil && l.mongo == nil
}

// Close closes all database access listeners.
func (l *dbListeners) Close() {
	if l.postgres != nil {
		l.postgres.Close()
	}
	if l.mysql != nil {
		l.mysql.Close()
	}
	if l.tls != nil {
		l.tls.Close()
	}
	if l.mongo != nil {
		l.mongo.Close()
	}
}

// setupProxyListeners sets up web proxy listeners based on the configuration
func (process *TeleportProcess) setupProxyListeners(networkingConfig types.ClusterNetworkingConfig, accessPoint authclient.ProxyAccessPoint, clusterName string) (*proxyListeners, error) {
	cfg := process.Config
	process.logger.DebugContext(process.ExitContext(), "Setting up Proxy listeners", "web_address", cfg.Proxy.WebAddr.Addr, "tunnel_address", cfg.Proxy.ReverseTunnelListenAddr.Addr)
	var err error
	var listeners proxyListeners

	muxCAGetter := func(ctx context.Context, id types.CertAuthID, loadKeys bool) (types.CertAuthority, error) {
		return accessPoint.GetCertAuthority(ctx, id, loadKeys)
	}

	if !cfg.Proxy.SSHAddr.IsEmpty() {
		l, err := process.importOrCreateListener(ListenerProxySSH, cfg.Proxy.SSHAddr.Addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		mux, err := multiplexer.New(multiplexer.Config{
			Listener:            l,
			PROXYProtocolMode:   cfg.Proxy.PROXYProtocolMode,
			ID:                  teleport.Component(teleport.ComponentProxy, "ssh"),
			CertAuthorityGetter: muxCAGetter,
			LocalClusterName:    clusterName,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		listeners.sshMux = mux
		listeners.ssh = mux.SSH()
		listeners.sshGRPC = mux.TLS()
		go func() {
			if err := mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
				process.logger.ErrorContext(process.ExitContext(), "Mux encountered error serving", "error", err, "mux_id", mux.ID)
			}
		}()
	}

	if cfg.Proxy.Kube.Enabled && !cfg.Proxy.Kube.ListenAddr.IsEmpty() {
		process.logger.DebugContext(process.ExitContext(), "Setup Proxy: turning on Kubernetes proxy.", "kube_address", cfg.Proxy.Kube.ListenAddr.Addr)
		listener, err := process.importOrCreateListener(ListenerProxyKube, cfg.Proxy.Kube.ListenAddr.Addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		listeners.kube = listener
	}

	if !cfg.Proxy.DisableDatabaseProxy {
		if !cfg.Proxy.MySQLAddr.IsEmpty() {
			process.logger.DebugContext(process.ExitContext(), "Setup Proxy: turning on MySQL proxy.", "mysql_address", cfg.Proxy.MySQLAddr.Addr)
			listener, err := process.importOrCreateListener(ListenerProxyMySQL, cfg.Proxy.MySQLAddr.Addr)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			listeners.db.mysql = listener
		}

		if !cfg.Proxy.MongoAddr.IsEmpty() {
			process.logger.DebugContext(process.ExitContext(), "Setup Proxy: turning on Mongo proxy.", "mongo_address", cfg.Proxy.MongoAddr.Addr)
			listener, err := process.importOrCreateListener(ListenerProxyMongo, cfg.Proxy.MongoAddr.Addr)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			listeners.db.mongo = listener
		}

		if !cfg.Proxy.PostgresAddr.IsEmpty() {
			process.logger.DebugContext(process.ExitContext(), "Setup Proxy: turning on Postgres proxy.", "postgres_address", cfg.Proxy.PostgresAddr.Addr)
			listener, err := process.importOrCreateListener(ListenerProxyPostgres, cfg.Proxy.PostgresAddr.Addr)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			listeners.db.postgres = listener
		}

	}

	tunnelStrategy, err := networkingConfig.GetTunnelStrategyType()
	if err != nil {
		process.logger.WarnContext(process.ExitContext(), "Failed to get tunnel strategy. Falling back to agent mesh strategy.", "error", err)
		tunnelStrategy = types.AgentMesh
	}

	if tunnelStrategy == types.ProxyPeering &&
		modules.GetModules().BuildType() != modules.BuildEnterprise {
		return nil, trace.AccessDenied("proxy peering is an enterprise-only feature")
	}

	if !cfg.Proxy.DisableReverseTunnel && tunnelStrategy == types.ProxyPeering {
		addr := process.Config.Proxy.PeerListenAddr()

		listener, err := process.importOrCreateListener(ListenerProxyPeer, addr.String())
		if err != nil {
			return nil, trace.Wrap(err)
		}

		listeners.proxyPeer = listener
	}

	switch {
	case cfg.Proxy.DisableWebService && cfg.Proxy.DisableReverseTunnel:
		process.logger.DebugContext(process.ExitContext(), "Setup Proxy: Reverse tunnel proxy and web proxy are disabled.")
		return &listeners, nil
	case cfg.Proxy.ReverseTunnelListenAddr == cfg.Proxy.WebAddr && !cfg.Proxy.DisableTLS:
		process.logger.DebugContext(process.ExitContext(), "Setup Proxy: Reverse tunnel proxy and web proxy listen on the same port, multiplexing is on.")
		listener, err := process.importOrCreateListener(ListenerProxyTunnelAndWeb, cfg.Proxy.WebAddr.Addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		listeners.mux, err = multiplexer.New(multiplexer.Config{
			PROXYProtocolMode:   cfg.Proxy.PROXYProtocolMode,
			Listener:            listener,
			ID:                  teleport.Component(teleport.ComponentProxy, "tunnel", "web", process.id),
			CertAuthorityGetter: muxCAGetter,
			LocalClusterName:    clusterName,
		})
		if err != nil {
			listener.Close()
			return nil, trace.Wrap(err)
		}
		if !cfg.Proxy.DisableWebService {
			listeners.web = listeners.mux.TLS()
		}
		process.muxPostgresOnWebPort(cfg, &listeners)
		if !cfg.Proxy.DisableReverseTunnel {
			listeners.reverseTunnel = listeners.mux.SSH()
		}
		go func() {
			if err := listeners.mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
				process.logger.ErrorContext(process.ExitContext(), "Mux encountered error serving", "error", err, "mux_id", listeners.mux.ID)
			}
		}()
		return &listeners, nil
	case cfg.Proxy.PROXYProtocolMode != multiplexer.PROXYProtocolOff && !cfg.Proxy.DisableWebService && !cfg.Proxy.DisableTLS:
		process.logger.DebugContext(process.ExitContext(), "Setup Proxy: PROXY protocol is enabled for web service, multiplexing is on.")
		listener, err := process.importOrCreateListener(ListenerProxyWeb, cfg.Proxy.WebAddr.Addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		listeners.mux, err = multiplexer.New(multiplexer.Config{
			PROXYProtocolMode:   cfg.Proxy.PROXYProtocolMode,
			Listener:            listener,
			ID:                  teleport.Component(teleport.ComponentProxy, "web", process.id),
			CertAuthorityGetter: muxCAGetter,
			LocalClusterName:    clusterName,
		})
		if err != nil {
			listener.Close()
			return nil, trace.Wrap(err)
		}
		listeners.web = listeners.mux.TLS()
		process.muxPostgresOnWebPort(cfg, &listeners)
		if !cfg.Proxy.ReverseTunnelListenAddr.IsEmpty() {
			if err := process.initMinimalReverseTunnelListener(cfg, &listeners); err != nil {
				listener.Close()
				listeners.Close()
				return nil, trace.Wrap(err)
			}
		}
		go func() {
			if err := listeners.mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
				process.logger.ErrorContext(process.ExitContext(), "Mux encountered error serving", "error", err, "mux_id", listeners.mux.ID)
			}
		}()
		return &listeners, nil
	default:
		process.logger.DebugContext(process.ExitContext(), "Setup Proxy: Proxy and reverse tunnel are listening on separate ports.")
		if !cfg.Proxy.DisableReverseTunnel && !cfg.Proxy.ReverseTunnelListenAddr.IsEmpty() {
			if cfg.Proxy.DisableWebService {
				listeners.reverseTunnel, err = process.importOrCreateListener(ListenerProxyTunnel, cfg.Proxy.ReverseTunnelListenAddr.Addr)
				if err != nil {
					listeners.Close()
					return nil, trace.Wrap(err)
				}
			} else {
				if err := process.initMinimalReverseTunnelListener(cfg, &listeners); err != nil {
					listeners.Close()
					return nil, trace.Wrap(err)
				}
			}
		}
		if !cfg.Proxy.DisableWebService && !cfg.Proxy.WebAddr.IsEmpty() {
			listener, err := process.importOrCreateListener(ListenerProxyWeb, cfg.Proxy.WebAddr.Addr)
			if err != nil {
				listeners.Close()
				return nil, trace.Wrap(err)
			}
			// Unless database proxy is explicitly disabled (which is currently
			// only done by tests and not exposed via file config), the web
			// listener is multiplexing both web and db client connections.
			if !cfg.Proxy.DisableDatabaseProxy && !cfg.Proxy.DisableTLS {
				process.logger.DebugContext(process.ExitContext(), "Setup Proxy: Multiplexing web and database proxy on the same port.")
				listeners.mux, err = multiplexer.New(multiplexer.Config{
					PROXYProtocolMode:   cfg.Proxy.PROXYProtocolMode,
					Listener:            listener,
					ID:                  teleport.Component(teleport.ComponentProxy, "web", process.id),
					CertAuthorityGetter: muxCAGetter,
					LocalClusterName:    clusterName,
				})
				if err != nil {
					listener.Close()
					listeners.Close()
					return nil, trace.Wrap(err)
				}
				listeners.web = listeners.mux.TLS()
				process.muxPostgresOnWebPort(cfg, &listeners)
				go func() {
					if err := listeners.mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
						process.logger.ErrorContext(process.ExitContext(), "Mux encountered error serving", "error", err, "mux_id", listeners.mux.ID)
					}
				}()
			} else {
				process.logger.DebugContext(process.ExitContext(), "Setup Proxy: TLS is disabled, multiplexing is off.")
				listeners.web = listener
			}
		}

		// Even if web service API was disabled create a web listener used for ALPN/SNI service as the master port
		if cfg.Proxy.DisableWebService && !cfg.Proxy.DisableTLS && listeners.web == nil {
			listeners.web, err = process.importOrCreateListener(ListenerProxyWeb, cfg.Proxy.WebAddr.Addr)
			if err != nil {
				return nil, trace.Wrap(err)
			}
		}
		return &listeners, nil
	}
}

// initMinimalReverseTunnelListener starts a listener over a reverse tunnel that multiplexes a minimal subset of the
// web API.
func (process *TeleportProcess) initMinimalReverseTunnelListener(cfg *servicecfg.Config, listeners *proxyListeners) error {
	listener, err := process.importOrCreateListener(ListenerProxyTunnel, cfg.Proxy.ReverseTunnelListenAddr.Addr)
	if err != nil {
		return trace.Wrap(err)
	}
	listeners.reverseTunnelMux, err = multiplexer.New(multiplexer.Config{
		PROXYProtocolMode: cfg.Proxy.PROXYProtocolMode,
		Listener:          listener,
		ID:                teleport.Component(teleport.ComponentProxy, "tunnel", "web", process.id),
	})
	if err != nil {
		listener.Close()
		return trace.Wrap(err)
	}
	listeners.reverseTunnel = listeners.reverseTunnelMux.SSH()
	go func() {
		if err := listeners.reverseTunnelMux.Serve(); err != nil {
			process.logger.DebugContext(process.ExitContext(), "Minimal reverse tunnel mux exited with error", "error", err)
		}
	}()
	listeners.minimalWeb = listeners.reverseTunnelMux.TLS()
	return nil
}

// muxPostgresOnWebPort starts Postgres proxy listener multiplexed on Teleport Proxy web port,
// unless postgres_listen_addr was specified.
func (process *TeleportProcess) muxPostgresOnWebPort(cfg *servicecfg.Config, listeners *proxyListeners) {
	if !cfg.Proxy.DisableDatabaseProxy && cfg.Proxy.PostgresAddr.IsEmpty() {
		listeners.db.postgres = listeners.mux.DB()
	}
}

func (process *TeleportProcess) initProxyEndpoint(conn *Connector) error {
	// clean up unused descriptors passed for proxy, but not used by it
	defer func() {
		if err := process.closeImportedDescriptors(teleport.ComponentProxy); err != nil {
			process.logger.WarnContext(process.ExitContext(), "Failed closing imported file descriptors", "error", err)
		}
	}()
	var err error
	cfg := process.Config
	var tlsConfigWeb *tls.Config

	clusterName := conn.ClusterName()

	proxyLimiter, err := limiter.NewLimiter(cfg.Proxy.Limiter)
	if err != nil {
		return trace.Wrap(err)
	}

	reverseTunnelLimiter, err := limiter.NewLimiter(cfg.Proxy.Limiter)
	if err != nil {
		return trace.Wrap(err)
	}

	// make a caching auth client for the auth server:
	accessPoint, err := process.newLocalCacheForProxy(conn.Client, []string{teleport.ComponentProxy})
	if err != nil {
		return trace.Wrap(err)
	}

	clusterNetworkConfig, err := accessPoint.GetClusterNetworkingConfig(process.ExitContext())
	if err != nil {
		return trace.Wrap(err)
	}

	listeners, err := process.setupProxyListeners(clusterNetworkConfig, accessPoint, clusterName)
	if err != nil {
		return trace.Wrap(err)
	}

	proxySSHAddr := cfg.Proxy.SSHAddr
	// override value of cfg.Proxy.SSHAddr with listener addr in order
	// to support binding to a random port (e.g. `127.0.0.1:0`).
	if listeners.ssh != nil {
		proxySSHAddr.Addr = listeners.ssh.Addr().String()
	}

	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id))

	// asyncEmitter makes sure that sessions do not block
	// in case if connections are slow
	asyncEmitter, err := process.NewAsyncEmitter(conn.Client)
	if err != nil {
		return trace.Wrap(err)
	}
	streamEmitter := &events.StreamerAndEmitter{
		Emitter:  asyncEmitter,
		Streamer: conn.Client,
	}

	lockWatcher, err := services.NewLockWatcher(process.ExitContext(), services.LockWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component: teleport.ComponentProxy,
			Logger:    process.logger.With(teleport.ComponentKey, teleport.ComponentProxy),
			Client:    conn.Client,
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	nodeWatcher, err := services.NewNodeWatcher(process.ExitContext(), services.NodeWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component:    teleport.ComponentProxy,
			Logger:       process.logger.With(teleport.ComponentKey, teleport.ComponentProxy),
			Client:       accessPoint,
			MaxStaleness: time.Minute,
		},
		NodesGetter: accessPoint,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	gitServerWatcher, err := services.NewGitServerWatcher(process.ExitContext(), services.GitServerWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component:    teleport.ComponentProxy,
			Logger:       process.logger.With(teleport.ComponentKey, teleport.ComponentProxy),
			Client:       accessPoint,
			MaxStaleness: time.Minute,
		},
		GitServerGetter: accessPoint.GitServerReadOnlyClient(),
	})
	if err != nil {
		return trace.Wrap(err)
	}

	caWatcher, err := services.NewCertAuthorityWatcher(process.ExitContext(), services.CertAuthorityWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component: teleport.ComponentProxy,
			Logger:    process.logger.With(teleport.ComponentKey, teleport.ComponentProxy),
			Client:    accessPoint,
		},
		AuthorityGetter: accessPoint,
		Types: []types.CertAuthType{
			types.HostCA,
			types.UserCA,
			types.DatabaseCA,
			types.OpenSSHCA,
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	serverTLSConfig, err := conn.ServerTLSConfig(cfg.CipherSuites)
	if err != nil {
		return trace.Wrap(err)
	}
	alpnRouter, reverseTunnelALPNRouter := setupALPNRouter(listeners, serverTLSConfig, cfg)
	alpnAddr := ""
	if listeners.alpn != nil {
		alpnAddr = listeners.alpn.Addr().String()
	}
	ingressReporter, err := ingress.NewReporter(alpnAddr)
	if err != nil {
		return trace.Wrap(err)
	}
	proxySigner, err := conn.getPROXYSigner(process.Clock, cfg.Proxy.PROXYAllowDowngrade)
	if err != nil {
		return trace.Wrap(err)
	}

	// We register the shutdown function before starting the services because we want to run it even if we encounter an
	// error and return early. Some of the registered services don't watch if the context is Done (e.g. proxy.web).
	// In case of error, if we don't run "proxy.shutdown", those registered services will run ad vitam aeternam and
	// Supervisor.Wait() won't return.
	var (
		tsrv                     reversetunnelclient.Server
		peerClient               *peer.Client
		peerQUICTransport        *quic.Transport
		rcWatcher                *reversetunnel.RemoteClusterTunnelManager
		peerServer               *peer.Server
		peerQUICServer           *peerquic.Server
		webServer                *web.Server
		minimalWebServer         *web.Server
		sshProxy                 *regular.Server
		sshGRPCServer            *grpc.Server
		kubeServer               *kubeproxy.TLSServer
		grpcServerPublic         *grpc.Server
		grpcServerMTLS           *grpc.Server
		alpnServer               *alpnproxy.Proxy
		reverseTunnelALPNServer  *alpnproxy.Proxy
		clientTLSConfigGenerator *auth.ClientTLSConfigGenerator
	)

	defer func() {
		// execute this when process is asked to exit:
		process.OnExit("proxy.shutdown", func(payload any) {
			// Close the listeners at the beginning of shutdown, because we are not
			// really guaranteed to be capable to serve new requests if we're
			// halfway through a shutdown, and double closing a listener is fine.
			listeners.Close()
			if payload == nil {
				logger.InfoContext(process.ExitContext(), "Shutting down immediately")
				if tsrv != nil {
					warnOnErr(process.ExitContext(), tsrv.Close(), logger)
				}
				if rcWatcher != nil {
					warnOnErr(process.ExitContext(), rcWatcher.Close(), logger)
				}
				if peerServer != nil {
					warnOnErr(process.ExitContext(), peerServer.Close(), logger)
				}
				if peerQUICServer != nil {
					warnOnErr(process.ExitContext(), peerQUICServer.Close(), logger)
				}
				if webServer != nil {
					warnOnErr(process.ExitContext(), webServer.Close(), logger)
				}
				if minimalWebServer != nil {
					warnOnErr(process.ExitContext(), minimalWebServer.Close(), logger)
				}
				if peerClient != nil {
					warnOnErr(process.ExitContext(), peerClient.Stop(), logger)
				}
				if sshProxy != nil {
					warnOnErr(process.ExitContext(), sshProxy.Close(), logger)
				}
				if sshGRPCServer != nil {
					sshGRPCServer.Stop()
				}
				if kubeServer != nil {
					warnOnErr(process.ExitContext(), kubeServer.Close(), logger)
				}
				if grpcServerPublic != nil {
					grpcServerPublic.Stop()
				}
				if grpcServerMTLS != nil {
					grpcServerMTLS.Stop()
				}
				if alpnServer != nil {
					warnOnErr(process.ExitContext(), alpnServer.Close(), logger)
				}
				if reverseTunnelALPNServer != nil {
					warnOnErr(process.ExitContext(), reverseTunnelALPNServer.Close(), logger)
				}

				if clientTLSConfigGenerator != nil {
					clientTLSConfigGenerator.Close()
				}
			} else {
				logger.InfoContext(process.ExitContext(), "Shutting down gracefully")
				ctx := payloadContext(payload)
				if tsrv != nil {
					warnOnErr(ctx, tsrv.DrainConnections(ctx), logger)
				}
				if sshProxy != nil {
					warnOnErr(ctx, sshProxy.Shutdown(ctx), logger)
				}
				if sshGRPCServer != nil {
					sshGRPCServer.GracefulStop()
				}
				if webServer != nil {
					warnOnErr(ctx, webServer.Shutdown(ctx), logger)
				}
				if minimalWebServer != nil {
					warnOnErr(ctx, minimalWebServer.Shutdown(ctx), logger)
				}
				if tsrv != nil {
					warnOnErr(ctx, tsrv.Shutdown(ctx), logger)
				}
				if rcWatcher != nil {
					warnOnErr(ctx, rcWatcher.Close(), logger)
				}
				if peerServer != nil {
					warnOnErr(ctx, peerServer.Shutdown(), logger)
				}
				if peerQUICServer != nil {
					warnOnErr(ctx, peerQUICServer.Shutdown(ctx), logger)
				}
				if peerClient != nil {
					peerClient.Shutdown(ctx)
				}
				if kubeServer != nil {
					warnOnErr(ctx, kubeServer.Shutdown(ctx), logger)
				}
				if grpcServerPublic != nil {
					grpcServerPublic.GracefulStop()
				}
				if grpcServerMTLS != nil {
					grpcServerMTLS.GracefulStop()
				}
				if alpnServer != nil {
					warnOnErr(ctx, alpnServer.Close(), logger)
				}
				if reverseTunnelALPNServer != nil {
					warnOnErr(ctx, reverseTunnelALPNServer.Close(), logger)
				}

				// Explicitly deleting proxy heartbeats helps the behavior of
				// reverse tunnel agents during rollouts, as otherwise they'll keep
				// trying to reach proxies until the heartbeats expire.
				if services.ShouldDeleteServerHeartbeatsOnShutdown(ctx) {
					if err := conn.Client.DeleteProxy(ctx, process.Config.HostUUID); err != nil {
						if !trace.IsNotFound(err) {
							logger.WarnContext(ctx, "Failed to delete heartbeat", "error", err)
						} else {
							logger.DebugContext(ctx, "Failed to delete heartbeat", "error", err)
						}
					}
				}

				if clientTLSConfigGenerator != nil {
					clientTLSConfigGenerator.Close()
				}
			}
			if peerQUICTransport != nil {
				_ = peerQUICTransport.Close()
				_ = peerQUICTransport.Conn.Close()
			}
			warnOnErr(process.ExitContext(), asyncEmitter.Close(), logger)
			warnOnErr(process.ExitContext(), conn.Close(), logger)
			logger.InfoContext(process.ExitContext(), "Exited")
		})
	}()

	// register SSH reverse tunnel server that accepts connections
	// from remote teleport nodes
	if !process.Config.Proxy.DisableReverseTunnel {
		if listeners.proxyPeer != nil {
			if process.Config.Proxy.QUICProxyPeering {
				// the stateless reset key is important in case there's a crash
				// so peers can be told to close their side of the connections
				// instead of having to wait for a timeout; for this reason, we
				// store it in the datadir, which should persist just as much as
				// the host ID and the cluster credentials
				resetKey, err := process.readOrInitPeerStatelessResetKey()
				if err != nil {
					return trace.Wrap(err)
				}
				pc, err := process.createPacketConn(string(ListenerProxyPeer), listeners.proxyPeer.Addr().String())
				if err != nil {
					return trace.Wrap(err)
				}
				peerQUICTransport = &quic.Transport{
					Conn: pc,

					StatelessResetKey: resetKey,
				}
			}

			peerClient, err = peer.NewClient(peer.ClientConfig{
				Context:           process.ExitContext(),
				ID:                process.Config.HostUUID,
				AuthClient:        conn.Client,
				AccessPoint:       accessPoint,
				TLSCipherSuites:   cfg.CipherSuites,
				GetTLSCertificate: conn.ClientGetCertificate,
				GetTLSRoots:       conn.ClientGetPool,
				Log:               process.logger,
				Clock:             process.Clock,
				ClusterName:       clusterName,
				QUICTransport:     peerQUICTransport,
			})
			if err != nil {
				return trace.Wrap(err)
			}
		}

		rtListener, err := reverseTunnelLimiter.WrapListener(listeners.reverseTunnel)
		if err != nil {
			return trace.Wrap(err)
		}

		tsrv, err = reversetunnel.NewServer(
			reversetunnel.Config{
				ClientTLSCipherSuites:   process.Config.CipherSuites,
				GetClientTLSCertificate: conn.ClientGetCertificate,
				Context:                 process.ExitContext(),
				Component:               teleport.Component(teleport.ComponentProxy, process.id),
				ID:                      process.Config.HostUUID,
				ClusterName:             clusterName,
				Listener:                rtListener,
				GetHostSigners:          conn.ServerGetHostSigners,
				LocalAuthClient:         conn.Client,
				LocalAccessPoint:        accessPoint,
				NewCachingAccessPoint:   process.newLocalCacheForRemoteProxy,
				Limiter:                 reverseTunnelLimiter,
				KeyGen:                  cfg.Keygen,
				Ciphers:                 cfg.Ciphers,
				KEXAlgorithms:           cfg.KEXAlgorithms,
				MACAlgorithms:           cfg.MACAlgorithms,
				DataDir:                 process.Config.DataDir,
				PollingPeriod:           process.Config.PollingPeriod,
				FIPS:                    cfg.FIPS,
				Emitter:                 streamEmitter,
				Logger:                  process.logger,
				LockWatcher:             lockWatcher,
				PeerClient:              peerClient,
				NodeWatcher:             nodeWatcher,
				GitServerWatcher:        gitServerWatcher,
				CertAuthorityWatcher:    caWatcher,
				CircuitBreakerConfig:    process.Config.CircuitBreakerConfig,
				LocalAuthAddresses:      utils.NetAddrsToStrings(process.Config.AuthServerAddresses()),
				IngressReporter:         ingressReporter,
				PROXYSigner:             proxySigner,
				EICEDialer:              awsoidc.DialInstance,
				EICESigner:              awsoidc.GenerateAndUploadKey,
			})
		if err != nil {
			return trace.Wrap(err)
		}
		process.RegisterCriticalFunc("proxy.reversetunnel.server", func() error {
			logger.InfoContext(process.ExitContext(), "Starting reverse tunnel server", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", cfg.Proxy.ReverseTunnelListenAddr.Addr, "cache_policy", process.Config.CachePolicy)
			if err := tsrv.Start(); err != nil {
				logger.ErrorContext(process.ExitContext(), "Failed starting reverse tunnel server", "error", err)
				return trace.Wrap(err)
			}

			// notify parties that we've started reverse tunnel server
			process.BroadcastEvent(Event{Name: ProxyReverseTunnelReady, Payload: tsrv})
			tsrv.Wait(process.ExitContext())
			return nil
		})
	}

	if !process.Config.Proxy.DisableTLS {
		tlsConfigWeb, err = process.setupProxyTLSConfig(conn, tsrv, accessPoint, clusterName)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	var proxyRouter *proxy.Router
	if !process.Config.Proxy.DisableReverseTunnel {
		router, err := proxy.NewRouter(proxy.RouterConfig{
			ClusterName:      clusterName,
			LocalAccessPoint: accessPoint,
			SiteGetter:       tsrv,
			TracerProvider:   process.TracingProvider,
			Logger:           process.logger.With(teleport.ComponentKey, "router"),
		})
		if err != nil {
			return trace.Wrap(err)
		}

		proxyRouter = router
	}

	// read the host UUID:
	serverID, err := hostid.ReadOrCreateFile(cfg.DataDir)
	if err != nil {
		return trace.Wrap(err)
	}

	sessionController, err := srv.NewSessionController(srv.SessionControllerConfig{
		Semaphores:     accessPoint,
		AccessPoint:    accessPoint,
		LockEnforcer:   lockWatcher,
		Emitter:        asyncEmitter,
		Component:      teleport.ComponentProxy,
		Logger:         process.logger.With(teleport.ComponentKey, "sessionctrl"),
		TracerProvider: process.TracingProvider,
		ServerID:       serverID,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// Register web proxy server
	alpnHandlerForWeb := &alpnproxy.ConnectionHandlerWrapper{}

	if !process.Config.Proxy.DisableWebService {
		var fs http.FileSystem
		if !process.Config.Proxy.DisableWebInterface {
			fs, err = newHTTPFileSystem()
			if err != nil {
				return trace.Wrap(err)
			}
		}

		proxySettings := &web.ProxySettings{
			ServiceConfig: cfg,
			ProxySSHAddr:  proxySSHAddr.String(),
			AccessPoint:   accessPoint,
		}

		proxyKubeAddr := cfg.Proxy.Kube.ListenAddr
		if len(cfg.Proxy.Kube.PublicAddrs) > 0 {
			proxyKubeAddr = cfg.Proxy.Kube.PublicAddrs[0]
		}

		traceClt := tracing.NewNoopClient()
		if cfg.Tracing.Enabled {
			traceConf, err := process.Config.Tracing.Config()
			if err != nil {
				return trace.Wrap(err)
			}
			traceConf.Logger = process.logger.With(teleport.ComponentKey, teleport.ComponentTracing)

			clt, err := tracing.NewStartedClient(process.ExitContext(), *traceConf)
			if err != nil {
				return trace.Wrap(err)
			}

			traceClt = clt
		}

		var accessGraphAddr utils.NetAddr
		if cfg.AccessGraph.Enabled {
			addr, err := utils.ParseAddr(cfg.AccessGraph.Addr)
			if err != nil {
				return trace.Wrap(err)
			}
			accessGraphAddr = *addr
		}

		cn, err := conn.Client.GetClusterName(process.ExitContext())
		if err != nil {
			return trace.Wrap(err)
		}

		lockWatcher, err := services.NewLockWatcher(process.GracefulExitContext(), services.LockWatcherConfig{
			ResourceWatcherConfig: services.ResourceWatcherConfig{
				Component: teleport.ComponentWebProxy,
				Logger:    process.logger,
				Client:    conn.Client,
				Clock:     process.Clock,
			},
		})
		if err != nil {
			return trace.Wrap(err)
		}

		authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
			ClusterName:   cn.GetClusterName(),
			AccessPoint:   accessPoint,
			LockWatcher:   lockWatcher,
			Logger:        process.logger,
			PermitCaching: process.Config.CachePolicy.Enabled,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		connMonitor, err := srv.NewConnectionMonitor(srv.ConnectionMonitorConfig{
			AccessPoint:    accessPoint,
			LockWatcher:    lockWatcher,
			Clock:          process.Clock,
			ServerID:       cfg.HostUUID,
			Emitter:        asyncEmitter,
			EmitterContext: process.GracefulExitContext(),
			Logger:         process.logger,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		connectionsHandler, err := app.NewConnectionsHandler(process.GracefulExitContext(), &app.ConnectionsHandlerConfig{
			Clock:             process.Clock,
			DataDir:           cfg.DataDir,
			Emitter:           asyncEmitter,
			Authorizer:        authorizer,
			HostID:            cfg.HostUUID,
			AuthClient:        conn.Client,
			AccessPoint:       accessPoint,
			TLSConfig:         serverTLSConfig,
			ConnectionMonitor: connMonitor,
			CipherSuites:      cfg.CipherSuites,
			ServiceComponent:  teleport.ComponentWebProxy,
			AWSConfigOptions: []awsconfig.OptionsFn{
				awsconfig.WithOIDCIntegrationClient(conn.Client),
				awsconfig.WithRolesAnywhereIntegrationClient(conn.Client),
			},
		})
		if err != nil {
			return trace.Wrap(err)
		}
		connectionsHandler.SetApplicationsProvider(func(ctx context.Context, publicAddr string) (types.Application, error) {
			allAppServers, err := accessPoint.GetApplicationServers(ctx, apidefaults.Namespace)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			publicAddressMatches := webapp.MatchPublicAddr(publicAddr)
			for _, a := range allAppServers {
				if publicAddressMatches(ctx, a) {
					return a.GetApp(), nil
				}
			}
			return nil, trace.NotFound("no app found for endpoint %q", publicAddr)
		})

		webConfig := web.Config{
			Proxy:                     tsrv,
			AuthServers:               cfg.AuthServerAddresses()[0],
			ProxyClient:               conn.Client,
			ProxySSHAddr:              proxySSHAddr,
			ProxyWebAddr:              cfg.Proxy.WebAddr,
			ProxyPublicAddrs:          cfg.Proxy.PublicAddrs,
			CipherSuites:              cfg.CipherSuites,
			FIPS:                      cfg.FIPS,
			AccessPoint:               accessPoint,
			Emitter:                   asyncEmitter,
			PluginRegistry:            process.PluginRegistry,
			HostUUID:                  process.Config.HostUUID,
			Context:                   process.GracefulExitContext(),
			StaticFS:                  fs,
			ClusterFeatures:           process.GetClusterFeatures(),
			GetProxyClientCertificate: conn.ClientGetCertificate,
			UI:                        cfg.Proxy.UI,
			ProxySettings:             proxySettings,
			PublicProxyAddr:           process.proxyPublicAddr().Addr,
			ALPNHandler:               alpnHandlerForWeb.HandleConnection,
			ProxyKubeAddr:             proxyKubeAddr,
			TraceClient:               traceClt,
			Router:                    proxyRouter,
			SessionControl: web.SessionControllerFunc(func(ctx context.Context, sctx *web.SessionContext, login, localAddr, remoteAddr string) (context.Context, error) {
				controller := srv.WebSessionController(sessionController)
				ctx, err := controller(ctx, sctx, login, localAddr, remoteAddr)
				return ctx, trace.Wrap(err)
			}),
			PROXYSigner:               proxySigner,
			NodeWatcher:               nodeWatcher,
			AccessGraphAddr:           accessGraphAddr,
			TracerProvider:            process.TracingProvider,
			AutomaticUpgradesChannels: cfg.Proxy.AutomaticUpgradesChannels,
			IntegrationAppHandler:     connectionsHandler,
			FeatureWatchInterval:      retryutils.HalfJitter(web.DefaultFeatureWatchInterval * 2),
			DatabaseREPLRegistry:      cfg.DatabaseREPLRegistry,
		}
		webHandler, err := web.NewHandler(webConfig)
		if err != nil {
			return trace.Wrap(err)
		}
		if !cfg.Proxy.DisableTLS && cfg.Proxy.DisableALPNSNIListener {
			listeners.tls, err = multiplexer.NewWebListener(multiplexer.WebListenerConfig{
				Listener: tls.NewListener(listeners.web, tlsConfigWeb),
			})
			if err != nil {
				return trace.Wrap(err)
			}
			listeners.web = listeners.tls.Web()
			listeners.db.tls = listeners.tls.DB()

			process.RegisterCriticalFunc("proxy.tls", func() error {
				logger.InfoContext(process.ExitContext(), "TLS multiplexer is starting.", "listen_address", cfg.Proxy.WebAddr.Addr)
				if err := listeners.tls.Serve(); !trace.IsConnectionProblem(err) {
					logger.WarnContext(process.ExitContext(), "TLS multiplexer error.", "error", err)
				}
				logger.InfoContext(process.ExitContext(), "TLS multiplexer exited.")
				return nil
			})
		}

		webServer, err = web.NewServer(web.ServerConfig{
			Server: &http.Server{
				Handler: utils.ChainHTTPMiddlewares(
					webHandler,
					limiter.MakeMiddleware(proxyLimiter),
					httplib.MakeTracingMiddleware(teleport.ComponentProxy),
					makeXForwardedForMiddleware(cfg),
				),
				// Note: read/write timeouts *should not* be set here because it
				// will break some application access use-cases.
				ReadHeaderTimeout: defaults.ReadHeadersTimeout,
				IdleTimeout:       apidefaults.DefaultIdleTimeout,
				ConnState:         ingress.HTTPConnStateReporter(ingress.Web, ingressReporter),
				ConnContext: func(ctx context.Context, c net.Conn) context.Context {
					ctx = authz.ContextWithConn(ctx, c)
					return authz.ContextWithClientAddrs(ctx, c.RemoteAddr(), c.LocalAddr())
				},
			},
			Handler: webHandler,
			Log:     process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
		})
		if err != nil {
			return trace.Wrap(err)
		}

		process.RegisterCriticalFunc("proxy.web", func() error {
			logger.InfoContext(process.ExitContext(), "Starting web proxy service.", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", cfg.Proxy.WebAddr.Addr)
			defer webHandler.Close()
			process.BroadcastEvent(Event{Name: ProxyWebServerReady, Payload: webHandler})
			if err := webServer.Serve(listeners.web); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				logger.WarnContext(process.ExitContext(), "Error while serving web requests", "error", err)
			}
			logger.InfoContext(process.ExitContext(), "Exited.")
			return nil
		})

		if listeners.reverseTunnelMux != nil {
			if minimalWebServer, err = process.initMinimalReverseTunnel(listeners, tlsConfigWeb, cfg, webConfig); err != nil {
				return trace.Wrap(err)
			}
		}
	} else {
		logger.InfoContext(process.ExitContext(), "Web UI is disabled.")
	}

	// Register ALPN handler that will be accepting connections for plain
	// TCP applications.
	// Use the same handler for MCP protocols, for now.
	if alpnRouter != nil {
		alpnRouter.Add(alpnproxy.HandlerDecs{
			MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolTCP),
			Handler:   webServer.HandleConnection,
		})
		alpnRouter.Add(alpnproxy.HandlerDecs{
			MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolMCP),
			Handler:   webServer.HandleConnection,
		})
	}

	var peerAddrString string
	if !process.Config.Proxy.DisableReverseTunnel && listeners.proxyPeer != nil {
		peerAddr, err := process.Config.Proxy.PublicPeerAddr()
		if err != nil {
			return trace.Wrap(err)
		}
		peerAddrString = peerAddr.String()

		peerServer, err = peer.NewServer(peer.ServerConfig{
			Log:          process.logger,
			Dialer:       reversetunnelclient.NewPeerDialer(tsrv),
			CipherSuites: cfg.CipherSuites,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return conn.serverGetCertificate()
			},
			GetClientCAs: func(chi *tls.ClientHelloInfo) (*x509.CertPool, error) {
				pool, _, err := authclient.ClientCertPool(chi.Context(), accessPoint, clusterName, types.HostCA)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				return pool, nil
			},
		})
		if err != nil {
			return trace.Wrap(err)
		}

		process.RegisterCriticalFunc("proxy.peer", func() error {
			if _, err := process.WaitForEvent(process.ExitContext(), ProxyReverseTunnelReady); err != nil {
				logger.DebugContext(process.ExitContext(), "Process exiting: failed to start peer proxy service waiting for reverse tunnel server.")
				return nil
			}

			logger.InfoContext(process.ExitContext(), "Starting peer proxy service.", "listen_address", logutils.StringerAttr(listeners.proxyPeer.Addr()))
			err := peerServer.Serve(listeners.proxyPeer)
			if err != nil {
				return trace.Wrap(err)
			}

			return nil
		})

		if peerQUICTransport != nil {
			peerQUICServer, err := peerquic.NewServer(peerquic.ServerConfig{
				Log:    process.logger,
				Dialer: reversetunnelclient.NewPeerDialer(tsrv),
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return conn.serverGetCertificate()
				},
				GetClientCAs: func(chi *tls.ClientHelloInfo) (*x509.CertPool, error) {
					pool, _, err := authclient.ClientCertPool(chi.Context(), accessPoint, clusterName, types.HostCA)
					if err != nil {
						return nil, trace.Wrap(err)
					}
					return pool, nil
				},
			})
			if err != nil {
				return trace.Wrap(err)
			}

			process.RegisterCriticalFunc("proxy.peer.quic", func() error {
				if _, err := process.WaitForEvent(process.ExitContext(), ProxyReverseTunnelReady); err != nil {
					logger.DebugContext(process.ExitContext(), "process exiting: failed to start QUIC peer proxy service waiting for reverse tunnel server")
					return nil
				}

				logger.InfoContext(process.ExitContext(), "starting QUIC peer proxy service", "local_addr", logutils.StringerAttr(peerQUICTransport.Conn.LocalAddr()))
				err := peerQUICServer.Serve(peerQUICTransport)
				if err != nil && !errors.Is(err, quic.ErrServerClosed) {
					return trace.Wrap(err)
				}

				return nil
			})
		}
	}

	staticLabels := make(map[string]string, 3)
	if cfg.Proxy.ProxyGroupID != "" {
		staticLabels[types.ProxyGroupIDLabel] = cfg.Proxy.ProxyGroupID
	}
	if cfg.Proxy.ProxyGroupGeneration != 0 {
		staticLabels[types.ProxyGroupGenerationLabel] = strconv.FormatUint(cfg.Proxy.ProxyGroupGeneration, 10)
	}
	if len(staticLabels) > 0 {
		logger.InfoContext(process.ExitContext(), "Enabling proxy group labels.", "group_id", cfg.Proxy.ProxyGroupID, "generation", cfg.Proxy.ProxyGroupGeneration)
	}
	if peerQUICTransport != nil {
		staticLabels[types.UnstableProxyPeerQUICLabel] = "yes"
		logger.InfoContext(process.ExitContext(), "advertising proxy peering QUIC support")
	}

	sshProxy, err = regular.New(
		process.ExitContext(),
		cfg.SSH.Addr,
		cfg.Hostname,
		conn.ServerGetHostSigners,
		accessPoint,
		cfg.DataDir,
		"",
		process.proxyPublicAddr(),
		conn.Client,
		regular.SetLimiter(proxyLimiter),
		regular.SetProxyMode(peerAddrString, tsrv, accessPoint, proxyRouter),
		regular.SetCiphers(cfg.Ciphers),
		regular.SetKEXAlgorithms(cfg.KEXAlgorithms),
		regular.SetMACAlgorithms(cfg.MACAlgorithms),
		regular.SetNamespace(apidefaults.Namespace),
		regular.SetRotationGetter(process.GetRotation),
		regular.SetFIPS(cfg.FIPS),
		regular.SetOnHeartbeat(process.OnHeartbeat(teleport.ComponentProxy)),
		regular.SetEmitter(streamEmitter),
		regular.SetLockWatcher(lockWatcher),
		// Allow Node-wide file copying checks to succeed so they can be
		// accurately checked later when an SCP/SFTP request hits the
		// destination Node.
		regular.SetAllowFileCopying(true),
		regular.SetTracerProvider(process.TracingProvider),
		regular.SetSessionController(sessionController),
		regular.SetConnectedProxyGetter(reversetunnel.NewConnectedProxyGetter()),
		regular.SetIngressReporter(ingress.SSH, ingressReporter),
		regular.SetPROXYSigner(proxySigner),
		regular.SetPublicAddrs(cfg.Proxy.PublicAddrs),
		regular.SetLabels(staticLabels, services.CommandLabels(nil), labels.Importer(nil)),
	)
	if err != nil {
		return trace.Wrap(err)
	}

	authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
		ClusterName:   clusterName,
		AccessPoint:   accessPoint,
		LockWatcher:   lockWatcher,
		Logger:        process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
		PermitCaching: process.Config.CachePolicy.Enabled,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// authMiddleware authenticates request assuming TLS client authentication
	// adds authentication information to the context
	// and passes it to the API server
	authMiddleware := &authz.Middleware{
		ClusterName: clusterName,
	}

	sshGRPCTLSConfig := serverTLSConfig.Clone()
	sshGRPCTLSConfig.NextProtos = []string{string(alpncommon.ProtocolHTTP2), string(alpncommon.ProtocolProxySSHGRPC)}
	sshGRPCTLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
	if lib.IsInsecureDevMode() {
		sshGRPCTLSConfig.InsecureSkipVerify = true
		sshGRPCTLSConfig.ClientAuth = tls.RequireAnyClientCert
	}

	// clientTLSConfigGenerator pre-generates specialized per-cluster client TLS config values
	clientTLSConfigGenerator, err = auth.NewClientTLSConfigGenerator(auth.ClientTLSConfigGeneratorConfig{
		TLS:                  sshGRPCTLSConfig,
		ClusterName:          clusterName,
		PermitRemoteClusters: true,
		AccessPoint:          accessPoint,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	process.OnExit("tls.config.generator", func(a any) {
		clientTLSConfigGenerator.Close()
	})

	sshGRPCTLSConfig.GetConfigForClient = clientTLSConfigGenerator.GetConfigForClient

	sshGRPCCreds, err := auth.NewTransportCredentials(auth.TransportCredentialsConfig{
		TransportCredentials: credentials.NewTLS(sshGRPCTLSConfig),
		UserGetter:           authMiddleware,
		Authorizer:           authorizer,
		GetAuthPreference:    accessPoint.GetAuthPreference,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	sshGRPCServer = grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(
			interceptors.GRPCServerUnaryErrorInterceptor,
		),
		grpc.ChainStreamInterceptor(
			interceptors.GRPCServerStreamErrorInterceptor,
		),
		grpc.Creds(sshGRPCCreds),
		grpc.MaxConcurrentStreams(defaults.GRPCMaxConcurrentStreams),
	)

	connMonitor, err := srv.NewConnectionMonitor(srv.ConnectionMonitorConfig{
		AccessPoint:    accessPoint,
		LockWatcher:    lockWatcher,
		Clock:          process.Clock,
		ServerID:       serverID,
		Emitter:        asyncEmitter,
		EmitterContext: process.ExitContext(),
		Logger:         process.logger,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	transportService, err := transportv1.NewService(transportv1.ServerConfig{
		FIPS:   cfg.FIPS,
		Logger: process.logger.With(teleport.ComponentKey, "transport"),
		Dialer: proxyRouter,
		SignerFn: func(authzCtx *authz.Context, clusterName string) agentless.SignerCreator {
			return agentless.SignerFromAuthzContext(authzCtx, accessPoint, clusterName)
		},
		ConnectionMonitor: connMonitor,
		LocalAddr:         listeners.sshGRPC.Addr(),
	})
	if err != nil {
		return trace.Wrap(err)
	}
	transportpb.RegisterTransportServiceServer(sshGRPCServer, transportService)

	process.RegisterCriticalFunc("proxy.ssh", func() error {
		sshListenerAddr := listeners.ssh.Addr().String()
		if cfg.Proxy.SSHAddr.Addr != "" {
			sshListenerAddr = cfg.Proxy.SSHAddr.Addr
		}
		logger.InfoContext(process.ExitContext(), " Starting SSH proxy service", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", sshListenerAddr)

		// start ssh server
		go func() {
			listener, err := proxyLimiter.WrapListener(listeners.ssh)
			if err != nil {
				logger.ErrorContext(process.ExitContext(), "Failed to set up SSH proxy server", "error", err)
				return
			}
			if err := sshProxy.Serve(listener); err != nil && !utils.IsOKNetworkError(err) {
				logger.ErrorContext(process.ExitContext(), "SSH proxy server terminated unexpectedly", "error", err)
			}
		}()

		// start grpc server
		go func() {
			listener, err := proxyLimiter.WrapListener(listeners.sshGRPC)
			if err != nil {
				logger.ErrorContext(process.ExitContext(), "Failed to set up SSH proxy server", "error", err)
				return
			}
			if err := sshGRPCServer.Serve(listener); err != nil && !utils.IsOKNetworkError(err) && !errors.Is(err, grpc.ErrServerStopped) {
				logger.ErrorContext(process.ExitContext(), "SSH gRPC server terminated unexpectedly", "error", err)
			}
		}()

		// broadcast that the proxy ssh server has started
		process.BroadcastEvent(Event{Name: ProxySSHReady, Payload: nil})
		return nil
	})

	rcWatchLog := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelAgent, process.id))

	// Create and register reverse tunnel AgentPool.
	rcWatcher, err = reversetunnel.NewRemoteClusterTunnelManager(reversetunnel.RemoteClusterTunnelManagerConfig{
		HostUUID:            conn.HostID(),
		AuthClient:          conn.Client,
		AccessPoint:         accessPoint,
		AuthMethods:         conn.ClientAuthMethods(),
		LocalCluster:        clusterName,
		KubeDialAddr:        utils.DialAddrFromListenAddr(kubeDialAddr(cfg.Proxy, clusterNetworkConfig.GetProxyListenerMode())),
		ReverseTunnelServer: tsrv,
		FIPS:                process.Config.FIPS,
		Logger:              rcWatchLog,
		LocalAuthAddresses:  utils.NetAddrsToStrings(process.Config.AuthServerAddresses()),
		PROXYSigner:         proxySigner,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.RegisterCriticalFunc("proxy.reversetunnel.watcher", func() error {
		rcWatchLog.InfoContext(process.ExitContext(), "Starting reverse tunnel agent pool")
		done := make(chan struct{})
		go func() {
			defer close(done)
			rcWatcher.Run(process.ExitContext())
		}()
		process.BroadcastEvent(Event{Name: ProxyAgentPoolReady, Payload: rcWatcher})
		<-done
		return nil
	})

	if listeners.kube != nil && !process.Config.Proxy.DisableReverseTunnel {
		authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
			ClusterName:   clusterName,
			AccessPoint:   accessPoint,
			LockWatcher:   lockWatcher,
			Logger:        process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
			PermitCaching: process.Config.CachePolicy.Enabled,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		// Register TLS endpoint of the Kube proxy service
		component := teleport.Component(teleport.ComponentProxy, teleport.ComponentProxyKube)
		kubeServiceType := kubeproxy.ProxyService
		if cfg.Proxy.Kube.LegacyKubeProxy {
			kubeServiceType = kubeproxy.LegacyProxyService
		}

		// kubeServerWatcher is used to watch for changes in the Kubernetes servers
		// and feed them to the kube proxy server so it can route the requests to
		// the correct kubernetes server.
		kubeServerWatcher, err := services.NewKubeServerWatcher(process.ExitContext(), services.KubeServerWatcherConfig{
			ResourceWatcherConfig: services.ResourceWatcherConfig{
				Component: component,
				Logger:    process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
				Client:    accessPoint,
			},
			KubernetesServerGetter: accessPoint,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		kubeServer, err = kubeproxy.NewTLSServer(kubeproxy.TLSServerConfig{
			ForwarderConfig: kubeproxy.ForwarderConfig{
				Namespace:                     apidefaults.Namespace,
				Keygen:                        cfg.Keygen,
				ClusterName:                   clusterName,
				ReverseTunnelSrv:              tsrv,
				Authz:                         authorizer,
				AuthClient:                    conn.Client,
				Emitter:                       asyncEmitter,
				DataDir:                       cfg.DataDir,
				CachingAuthClient:             accessPoint,
				HostID:                        cfg.HostUUID,
				ClusterOverride:               cfg.Proxy.Kube.ClusterOverride,
				KubeconfigPath:                cfg.Proxy.Kube.KubeconfigPath,
				Component:                     component,
				KubeServiceType:               kubeServiceType,
				LockWatcher:                   lockWatcher,
				CheckImpersonationPermissions: cfg.Kube.CheckImpersonationPermissions,
				PROXYSigner:                   proxySigner,
				// ConnTLSConfig is used by the proxy authenticate to the upstream kubernetes
				// services or remote clustes to be able to send the client identity
				// using Impersonation headers. The upstream service will validate if
				// the provided connection certificate is from a proxy server and
				// will impersonate the identity of the user that is making the request.
				GetConnTLSCertificate: conn.ClientGetCertificate,
				GetConnTLSRoots:       conn.ClientGetPool,
				ConnTLSCipherSuites:   cfg.CipherSuites,
				ClusterFeatures:       process.GetClusterFeatures,
			},
			TLS:                      serverTLSConfig.Clone(),
			LimiterConfig:            cfg.Proxy.Limiter,
			AccessPoint:              accessPoint,
			GetRotation:              process.GetRotation,
			OnHeartbeat:              process.OnHeartbeat(component),
			Log:                      process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
			IngressReporter:          ingressReporter,
			KubernetesServersWatcher: kubeServerWatcher,
			PROXYProtocolMode:        cfg.Proxy.PROXYProtocolMode,
			InventoryHandle:          process.inventoryHandle,
			ConnectedProxyGetter:     reversetunnel.NewConnectedProxyGetter(),
		})
		if err != nil {
			return trace.Wrap(err)
		}
		process.RegisterCriticalFunc("proxy.kube", func() error {
			logger := process.logger.With(teleport.ComponentKey, component)

			kubeListenAddr := listeners.kube.Addr().String()
			if cfg.Proxy.Kube.ListenAddr.Addr != "" {
				kubeListenAddr = cfg.Proxy.Kube.ListenAddr.Addr
			}
			logger.InfoContext(process.ExitContext(), "Starting Kube proxy.", "listen_address", kubeListenAddr)

			var mopts []kubeproxy.ServeOption
			if cfg.Testing.KubeMultiplexerIgnoreSelfConnections {
				mopts = append(mopts, kubeproxy.WithMultiplexerIgnoreSelfConnections())
			}

			err := kubeServer.Serve(listeners.kube, mopts...)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.WarnContext(process.ExitContext(), "Kube TLS server exited with error.", "error", err)
			}
			return nil
		})
	}

	// Start the database proxy server that will be accepting connections from
	// the database clients (such as psql or mysql), authenticating them, and
	// then routing them to a respective database server over the reverse tunnel
	// framework.
	if (!listeners.db.Empty() || alpnRouter != nil) && !process.Config.Proxy.DisableReverseTunnel {
		authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
			ClusterName:   clusterName,
			AccessPoint:   accessPoint,
			LockWatcher:   lockWatcher,
			Logger:        process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id)),
			PermitCaching: process.Config.CachePolicy.Enabled,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		connLimiter, err := limiter.NewLimiter(process.Config.Databases.Limiter)
		if err != nil {
			return trace.Wrap(err)
		}

		connMonitor, err := srv.NewConnectionMonitor(srv.ConnectionMonitorConfig{
			AccessPoint:    accessPoint,
			LockWatcher:    lockWatcher,
			Clock:          process.Config.Clock,
			ServerID:       process.Config.HostUUID,
			Emitter:        asyncEmitter,
			EmitterContext: process.ExitContext(),
			Logger:         process.logger,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		dbProxyServer, err := db.NewProxyServer(process.ExitContext(),
			db.ProxyServerConfig{
				AuthClient:         conn.Client,
				AccessPoint:        accessPoint,
				Authorizer:         authorizer,
				Tunnel:             tsrv,
				TLSConfig:          serverTLSConfig.Clone(),
				Limiter:            connLimiter,
				IngressReporter:    ingressReporter,
				ConnectionMonitor:  connMonitor,
				MySQLServerVersion: process.Config.Proxy.MySQLServerVersion,
			})
		if err != nil {
			return trace.Wrap(err)
		}

		if alpnRouter != nil && !cfg.Proxy.DisableDatabaseProxy {
			alpnRouter.Add(alpnproxy.HandlerDecs{
				MatchFunc:           alpnproxy.MatchByALPNPrefix(string(alpncommon.ProtocolMySQL)),
				HandlerWithConnInfo: alpnproxy.ExtractMySQLEngineVersion(dbProxyServer.MySQLProxy().HandleConnection),
			})
			alpnRouter.Add(alpnproxy.HandlerDecs{
				MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolMySQL),
				Handler:   dbProxyServer.MySQLProxy().HandleConnection,
			})
			alpnRouter.Add(alpnproxy.HandlerDecs{
				MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolPostgres),
				Handler:   dbProxyServer.PostgresProxy().HandleConnection,
			})
			alpnRouter.Add(alpnproxy.HandlerDecs{
				// For the following protocols ALPN Proxy will handle the
				// connection internally (terminate wrapped TLS traffic) and
				// route extracted connection to ALPN Proxy DB TLS Handler.
				MatchFunc: alpnproxy.MatchByProtocol(
					alpncommon.ProtocolMongoDB,
					alpncommon.ProtocolOracle,
					alpncommon.ProtocolRedisDB,
					alpncommon.ProtocolSnowflake,
					alpncommon.ProtocolSQLServer,
					alpncommon.ProtocolCassandra,
					alpncommon.ProtocolSpanner,
				),
			})
		}

		logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentDatabase))
		if listeners.db.postgres != nil {
			process.RegisterCriticalFunc("proxy.db.postgres", func() error {
				logger.InfoContext(process.ExitContext(), "Starting Database Postgres proxy server.", "listen_address", listeners.db.postgres.Addr())
				if err := dbProxyServer.ServePostgres(listeners.db.postgres); err != nil {
					logger.WarnContext(process.ExitContext(), "Postgres proxy server exited with error.", "error", err)
				}
				return nil
			})
		}
		if listeners.db.mysql != nil {
			process.RegisterCriticalFunc("proxy.db.mysql", func() error {
				logger.InfoContext(process.ExitContext(), "Starting MySQL proxy server.", "listen_address", cfg.Proxy.MySQLAddr.Addr)
				if err := dbProxyServer.ServeMySQL(listeners.db.mysql); err != nil {
					logger.WarnContext(process.ExitContext(), "MySQL proxy server exited with error.", "error", err)
				}
				return nil
			})
		}
		if listeners.db.tls != nil {
			process.RegisterCriticalFunc("proxy.db.tls", func() error {
				logger.InfoContext(process.ExitContext(), "Starting Database TLS proxy server.", "listen_address", cfg.Proxy.WebAddr.Addr)
				if err := dbProxyServer.ServeTLS(listeners.db.tls); err != nil {
					logger.WarnContext(process.ExitContext(), "Database TLS proxy server exited with error.", "error", err)
				}
				return nil
			})
		}

		if listeners.db.mongo != nil {
			process.RegisterCriticalFunc("proxy.db.mongo", func() error {
				logger.InfoContext(process.ExitContext(), "Starting Database Mongo proxy server.", "listen_address", cfg.Proxy.MongoAddr.Addr)
				if err := dbProxyServer.ServeMongo(listeners.db.mongo, tlsConfigWeb); err != nil {
					logger.WarnContext(process.ExitContext(), "Database Mongo proxy server exited with error.", "error", err)
				}
				return nil
			})
		}
	}

	if alpnRouter != nil {
		grpcServerPublic, err = process.initPublicGRPCServer(proxyLimiter, conn, listeners.grpcPublic)
		if err != nil {
			return trace.Wrap(err)
		}

		grpcServerMTLS, err = process.initSecureGRPCServer(
			initSecureGRPCServerCfg{
				limiter:  proxyLimiter,
				conn:     conn,
				listener: listeners.grpcMTLS,
				kubeProxyAddr: kubeDialAddr(
					cfg.Proxy,
					clusterNetworkConfig.GetProxyListenerMode(),
				),
				accessPoint:     accessPoint,
				lockWatcher:     lockWatcher,
				emitter:         asyncEmitter,
				tlsCipherSuites: cfg.CipherSuites,
			},
		)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	if !cfg.Proxy.DisableTLS && !cfg.Proxy.DisableALPNSNIListener && listeners.web != nil {
		authDialerService := alpnproxyauth.NewAuthProxyDialerService(
			tsrv,
			clusterName,
			utils.NetAddrsToStrings(process.Config.AuthServerAddresses()),
			proxySigner,
			process.TracingProvider.Tracer(teleport.ComponentProxy))

		alpnRouter.Add(alpnproxy.HandlerDecs{
			MatchFunc:           alpnproxy.MatchByALPNPrefix(string(alpncommon.ProtocolAuth)),
			HandlerWithConnInfo: authDialerService.HandleConnection,
			ForwardTLS:          true,
		})
		alpnServer, err = alpnproxy.New(alpnproxy.ProxyConfig{
			WebTLSConfig:      tlsConfigWeb.Clone(),
			IdentityTLSConfig: serverTLSConfig,
			Router:            alpnRouter,
			Listener:          listeners.alpn,
			ClusterName:       clusterName,
			AccessPoint:       accessPoint,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		alpnTLSConfigForWeb, err := process.setupALPNTLSConfigForWeb(serverTLSConfig, accessPoint, clusterName)
		if err != nil {
			return trace.Wrap(err)
		}
		alpnHandlerForWeb.Set(alpnServer.MakeConnectionHandler(alpnTLSConfigForWeb, alpncommon.ConnHandlerSourceWeb))

		process.RegisterCriticalFunc("proxy.tls.alpn.sni.proxy", func() error {
			logger.InfoContext(process.ExitContext(), "Starting TLS ALPN SNI proxy server on.", "listen_address", logutils.StringerAttr(listeners.alpn.Addr()))
			if err := alpnServer.Serve(process.ExitContext()); err != nil {
				logger.WarnContext(process.ExitContext(), "TLS ALPN SNI proxy proxy server exited with error.", "error", err)
			}
			return nil
		})

		if reverseTunnelALPNRouter != nil {
			reverseTunnelALPNServer, err = alpnproxy.New(alpnproxy.ProxyConfig{
				WebTLSConfig:      tlsConfigWeb.Clone(),
				IdentityTLSConfig: serverTLSConfig,
				Router:            reverseTunnelALPNRouter,
				Listener:          listeners.reverseTunnelALPN,
				ClusterName:       clusterName,
				AccessPoint:       accessPoint,
			})
			if err != nil {
				return trace.Wrap(err)
			}

			process.RegisterCriticalFunc("proxy.tls.alpn.sni.proxy.reverseTunnel", func() error {
				logger.InfoContext(process.ExitContext(), "Starting TLS ALPN SNI reverse tunnel proxy server.", "listen_address", listeners.reverseTunnelALPN.Addr())
				if err := reverseTunnelALPNServer.Serve(process.ExitContext()); err != nil {
					logger.WarnContext(process.ExitContext(), "TLS ALPN SNI proxy proxy on reverse tunnel server exited with error.", "error", err)
				}
				return nil
			})
		}
	}

	return nil
}

func (process *TeleportProcess) initMinimalReverseTunnel(listeners *proxyListeners, tlsConfigWeb *tls.Config, cfg *servicecfg.Config, webConfig web.Config) (*web.Server, error) {
	logger := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id))
	internalListener := listeners.minimalWeb
	if !cfg.Proxy.DisableTLS {
		internalListener = tls.NewListener(internalListener, tlsConfigWeb)
	}

	minimalListener, err := multiplexer.NewWebListener(multiplexer.WebListenerConfig{
		Listener: internalListener,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	listeners.minimalTLS = minimalListener

	minimalProxyLimiter, err := limiter.NewLimiter(cfg.Proxy.Limiter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	webConfig.MinimalReverseTunnelRoutesOnly = true
	minimalWebHandler, err := web.NewHandler(webConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	minimalProxyLimiter.WrapHandle(minimalWebHandler)

	process.RegisterCriticalFunc("proxy.reversetunnel.tls", func() error {
		logger.InfoContext(process.ExitContext(), "TLS multiplexer is starting.", "listen_address", cfg.Proxy.ReverseTunnelListenAddr.Addr)
		if err := minimalListener.Serve(); !trace.IsConnectionProblem(err) {
			logger.WarnContext(process.ExitContext(), "TLS multiplexer error.", "error", err)
		}
		logger.InfoContext(process.ExitContext(), "TLS multiplexer exited.")
		return nil
	})

	log := process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentReverseTunnelServer, process.id))

	minimalWebServer, err := web.NewServer(web.ServerConfig{
		Server: &http.Server{
			Handler:           httplib.MakeTracingHandler(minimalProxyLimiter, teleport.ComponentProxy),
			ReadTimeout:       apidefaults.DefaultIOTimeout,
			ReadHeaderTimeout: defaults.ReadHeadersTimeout,
			WriteTimeout:      apidefaults.DefaultIOTimeout,
			IdleTimeout:       apidefaults.DefaultIdleTimeout,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
		},
		Handler: minimalWebHandler,
		Log:     log,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	process.RegisterCriticalFunc("proxy.reversetunnel.web", func() error {
		logger.InfoContext(process.ExitContext(), "Minimal web proxy service is starting.", "version", teleport.Version, "git_ref", teleport.Gitref, "listen_address", cfg.Proxy.ReverseTunnelListenAddr.Addr)
		defer minimalWebHandler.Close()
		if err := minimalWebServer.Serve(minimalListener.Web()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.WarnContext(process.ExitContext(), "Error while serving web requests", "error", err)
		}
		logger.InfoContext(process.ExitContext(), "Exited.")
		return nil
	})

	return minimalWebServer, nil
}

func (process *TeleportProcess) readOrInitPeerStatelessResetKey() (*quic.StatelessResetKey, error) {
	resetKeyPath := filepath.Join(process.Config.DataDir, "peer_stateless_reset_key")
	k := new(quic.StatelessResetKey)
	stored, err := os.ReadFile(resetKeyPath)
	if err == nil && len(stored) == len(k) {
		copy(k[:], stored)
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		process.logger.WarnContext(process.ExitContext(), "Stateless reset key file unreadable or invalid.", "error", err)
	}
	if _, err := rand.Read(k[:]); err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	if err := renameio.WriteFile(resetKeyPath, k[:], 0o600); err != nil {
		process.logger.WarnContext(process.ExitContext(), "Failed to persist stateless reset key.", "error", err)
	}
	return k, nil
}

// kubeDialAddr returns Proxy Kube service address used for dialing local kube service
// by remote trusted cluster.
// If the proxy is running with Multiplex mode the WebPort is returned
// where connections are forwarded to kube service by ALPN SNI router.
func kubeDialAddr(config servicecfg.ProxyConfig, mode types.ProxyListenerMode) utils.NetAddr {
	if mode == types.ProxyListenerMode_Multiplex {
		return config.WebAddr
	}
	return config.Kube.ListenAddr
}

func (process *TeleportProcess) setupProxyTLSConfig(conn *Connector, tsrv reversetunnelclient.Server, accessPoint authclient.ReadProxyAccessPoint, clusterName string) (*tls.Config, error) {
	cfg := process.Config
	var tlsConfig *tls.Config
	acmeCfg := process.Config.Proxy.ACME
	if acmeCfg.Enabled {
		process.Config.Logger.InfoContext(process.ExitContext(), "Managing certs using ACME https://datatracker.ietf.org/doc/rfc8555/.")

		acmePath := filepath.Join(process.Config.DataDir, teleport.ComponentACME)
		if err := os.MkdirAll(acmePath, teleport.PrivateDirMode); err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		hostChecker, err := newHostPolicyChecker(hostPolicyCheckerConfig{
			publicAddrs: process.Config.Proxy.PublicAddrs,
			clt:         conn.Client,
			tun:         tsrv,
			clusterName: conn.ClusterName(),
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		m := &autocert.Manager{
			Cache:      autocert.DirCache(acmePath),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: hostChecker.checkHost,
			Email:      acmeCfg.Email,
		}
		if acmeCfg.URI != "" {
			m.Client = &acme.Client{DirectoryURL: acmeCfg.URI}
		}
		// We have to duplicate the behavior of `m.TLSConfig()` here because
		// http/1.1 needs to take precedence over h2 due to
		// https://bugs.chromium.org/p/chromium/issues/detail?id=1379017#c5 in Chrome.
		tlsConfig = &tls.Config{
			GetCertificate: m.GetCertificate,
			NextProtos: []string{
				string(alpncommon.ProtocolHTTP), string(alpncommon.ProtocolHTTP2), // enable HTTP/2
				acme.ALPNProto, // enable tls-alpn ACME challenges
			},
		}
		utils.SetupTLSConfig(tlsConfig, cfg.CipherSuites)
	} else {
		certReloader := NewCertReloader(CertReloaderConfig{
			KeyPairs:               process.Config.Proxy.KeyPairs,
			KeyPairsReloadInterval: process.Config.Proxy.KeyPairsReloadInterval,
		})
		if err := certReloader.Run(process.ExitContext()); err != nil {
			return nil, trace.Wrap(err)
		}

		tlsConfig = utils.TLSConfig(cfg.CipherSuites)
		tlsConfig.GetCertificate = certReloader.GetCertificate
	}

	setupTLSConfigALPNProtocols(tlsConfig)
	if err := process.setupTLSConfigClientCAGeneratorForCluster(tlsConfig, accessPoint, clusterName); err != nil {
		return nil, trace.Wrap(err)
	}
	return tlsConfig, nil
}

func setupTLSConfigALPNProtocols(tlsConfig *tls.Config) {
	// Go 1.17 introduced strict ALPN https://golang.org/doc/go1.17#ALPN If a client protocol is not recognized
	// the TLS handshake will fail.
	tlsConfig.NextProtos = apiutils.Deduplicate(append(tlsConfig.NextProtos, alpncommon.ProtocolsToString(alpncommon.SupportedProtocols)...))
}

func (process *TeleportProcess) setupTLSConfigClientCAGeneratorForCluster(tlsConfig *tls.Config, accessPoint authclient.ReadProxyAccessPoint, clusterName string) error {
	// create a local copy of the TLS config so we can change some settings that are only
	// relevant to the config returned by GetConfigForClient.
	tlsClone := tlsConfig.Clone()

	// Set client auth to "verify client cert if given" to support
	// app access CLI flow.
	//
	// Clients (like curl) connecting to the web proxy endpoint will
	// present a client certificate signed by the cluster's user CA.
	//
	// Browser connections to web UI and other clients (like database
	// access) connecting to web proxy won't be affected since they
	// don't present a certificate.
	tlsClone.ClientAuth = tls.VerifyClientCertIfGiven

	// Set up the client CA generator containing for the local cluster's CAs in
	// order to be able to validate certificates provided by app access CLI clients.
	generator, err := auth.NewClientTLSConfigGenerator(auth.ClientTLSConfigGeneratorConfig{
		TLS:                  tlsClone,
		ClusterName:          clusterName,
		PermitRemoteClusters: false,
		AccessPoint:          accessPoint,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.OnExit("tls.config.generator", func(payload any) {
		generator.Close()
	})

	// set getter on the original TLS config.
	tlsConfig.GetConfigForClient = generator.GetConfigForClient

	// note: generator will be closed via the passed in context, rather than an explicit call to Close.
	return nil
}

func (process *TeleportProcess) setupALPNTLSConfigForWeb(tlsConfig *tls.Config, accessPoint authclient.ReadProxyAccessPoint, clusterName string) (*tls.Config, error) {
	tlsConfig = tlsConfig.Clone()
	setupTLSConfigALPNProtocols(tlsConfig)
	if err := process.setupTLSConfigClientCAGeneratorForCluster(tlsConfig, accessPoint, clusterName); err != nil {
		return nil, trace.Wrap(err)
	}

	return tlsConfig, nil
}

func setupALPNRouter(listeners *proxyListeners, serverTLSConfig *tls.Config, cfg *servicecfg.Config) (router, rtRouter *alpnproxy.Router) {
	if listeners.web == nil || cfg.Proxy.DisableTLS || cfg.Proxy.DisableALPNSNIListener {
		return nil, nil
	}
	// ALPN proxy service will use web listener where listener.web will be overwritten by alpn wrapper
	// that allows to dispatch the http/1.1 and h2 traffic to webService.
	listeners.alpn = listeners.web
	router = alpnproxy.NewRouter()

	if listeners.minimalWeb != nil {
		listeners.reverseTunnelALPN = listeners.minimalWeb
		rtRouter = alpnproxy.NewRouter()
	}

	if cfg.Proxy.Kube.Enabled {
		kubeListener := alpnproxy.NewMuxListenerWrapper(listeners.kube, listeners.web)
		router.AddKubeHandler(kubeListener.HandleConnection)
		listeners.kube = kubeListener
	}
	if !cfg.Proxy.DisableReverseTunnel {
		reverseTunnel := alpnproxy.NewMuxListenerWrapper(listeners.reverseTunnel, listeners.web)
		router.Add(alpnproxy.HandlerDecs{
			MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolReverseTunnel),
			Handler:   reverseTunnel.HandleConnection,
		})
		listeners.reverseTunnel = reverseTunnel

		if rtRouter != nil {
			minimalWeb := alpnproxy.NewMuxListenerWrapper(nil, listeners.reverseTunnelALPN)
			rtRouter.Add(alpnproxy.HandlerDecs{
				MatchFunc: alpnproxy.MatchByProtocol(
					alpncommon.ProtocolHTTP,
					alpncommon.ProtocolHTTP2,
					alpncommon.ProtocolDefault,
				),
				Handler:    minimalWeb.HandleConnection,
				ForwardTLS: true,
			})
			listeners.minimalWeb = minimalWeb
		}

	}

	if !cfg.Proxy.DisableWebService {
		webWrapper := alpnproxy.NewMuxListenerWrapper(nil, listeners.web)
		router.Add(alpnproxy.HandlerDecs{
			MatchFunc: alpnproxy.MatchByProtocol(
				alpncommon.ProtocolHTTP,
				alpncommon.ProtocolHTTP2,
				acme.ALPNProto,
			),
			Handler:    webWrapper.HandleConnection,
			ForwardTLS: false,
		})
		listeners.web = webWrapper
	}
	// grpcPublicListener is a listener that does not enforce mTLS authentication.
	// It must not be used for any services that require authentication and currently
	// it is only used by the join service which nodes rely on to join the cluster.
	grpcPublicListener := alpnproxy.NewMuxListenerWrapper(nil /* serviceListener */, listeners.web)
	grpcPublicListener = alpnproxy.NewMuxListenerWrapper(grpcPublicListener, listeners.reverseTunnel)
	router.Add(alpnproxy.HandlerDecs{
		MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolProxyGRPCInsecure),
		Handler:   grpcPublicListener.HandleConnection,
	})
	if rtRouter != nil {
		rtRouter.Add(alpnproxy.HandlerDecs{
			MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolProxyGRPCInsecure),
			Handler:   grpcPublicListener.HandleConnection,
		})
	}
	listeners.grpcPublic = grpcPublicListener

	// grpcSecureListener is a listener that is used by a gRPC server that enforces
	// mTLS authentication. It must be used for any gRPC services that require authentication.
	grpcSecureListener := alpnproxy.NewMuxListenerWrapper(nil /* serviceListener */, listeners.web)
	router.Add(alpnproxy.HandlerDecs{
		MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolProxyGRPCSecure),
		Handler:   grpcSecureListener.HandleConnection,
		// Forward the TLS configuration to the gRPC server so that it can handle mTLS authentication.
		ForwardTLS: true,
	})
	listeners.grpcMTLS = grpcSecureListener

	sshProxyListener := alpnproxy.NewMuxListenerWrapper(listeners.ssh, listeners.web)

	proxySSHTLSConfig := serverTLSConfig.Clone()
	proxySSHTLSConfig.NextProtos = []string{string(alpncommon.ProtocolProxySSH)}
	router.Add(alpnproxy.HandlerDecs{
		MatchFunc: alpnproxy.MatchByProtocol(alpncommon.ProtocolProxySSH),
		Handler:   sshProxyListener.HandleConnection,
		TLSConfig: proxySSHTLSConfig,
	})
	listeners.ssh = sshProxyListener

	sshGRPCListener := alpnproxy.NewMuxListenerWrapper(listeners.sshGRPC, listeners.web)
	// TLS forwarding is used instead of providing the TLSConfig so that the
	// authentication information makes it into the gRPC credentials.
	router.Add(alpnproxy.HandlerDecs{
		MatchFunc:  alpnproxy.MatchByProtocol(alpncommon.ProtocolProxySSHGRPC),
		Handler:    sshGRPCListener.HandleConnection,
		ForwardTLS: true,
	})
	listeners.sshGRPC = sshGRPCListener

	webTLSDB := alpnproxy.NewMuxListenerWrapper(nil, listeners.web)
	router.AddDBTLSHandler(webTLSDB.HandleConnection)
	listeners.db.tls = webTLSDB

	return router, rtRouter
}

// waitForAppDepend waits until all dependencies for an application service
// are ready.
func (process *TeleportProcess) waitForAppDepend() {
	for _, event := range appDependEvents {
		_, err := process.WaitForEvent(process.ExitContext(), event)
		if err != nil {
			process.logger.DebugContext(process.ExitContext(), "Process is exiting.")
			break
		}
	}
}

// registerExpectedServices sets up the instance role -> identity event mapping.
func (process *TeleportProcess) registerExpectedServices(cfg *servicecfg.Config) {
	// Register additional expected services for this Teleport instance.
	// Meant for enterprise support.
	for _, r := range cfg.AdditionalExpectedRoles {
		process.SetExpectedInstanceRole(r.Role, r.IdentityEvent)
	}

	if cfg.Auth.Enabled {
		process.SetExpectedInstanceRole(types.RoleAuth, AuthIdentityEvent)
	}

	if cfg.SSH.Enabled || cfg.OpenSSH.Enabled {
		process.SetExpectedInstanceRole(types.RoleNode, SSHIdentityEvent)
	}

	if cfg.Proxy.Enabled {
		process.SetExpectedInstanceRole(types.RoleProxy, ProxyIdentityEvent)
	}

	if cfg.Relay.Enabled {
		process.SetExpectedInstanceRole(types.RoleRelay, RelayIdentityEvent)
	}

	if cfg.Kube.Enabled {
		process.SetExpectedInstanceRole(types.RoleKube, KubeIdentityEvent)
	}

	if cfg.Apps.Enabled {
		process.SetExpectedInstanceRole(types.RoleApp, AppsIdentityEvent)
	}

	if cfg.Databases.Enabled {
		process.SetExpectedInstanceRole(types.RoleDatabase, DatabasesIdentityEvent)
	}

	if cfg.WindowsDesktop.Enabled {
		process.SetExpectedInstanceRole(types.RoleWindowsDesktop, WindowsDesktopIdentityEvent)
	}

	if cfg.Discovery.Enabled {
		process.SetExpectedInstanceRole(types.RoleDiscovery, DiscoveryIdentityEvent)
	}
}

// appDependEvents is a list of events that the application service depends on.
var appDependEvents = []string{
	AuthTLSReady,
	AuthIdentityEvent,
	ProxySSHReady,
	ProxyWebServerReady,
	ProxyReverseTunnelReady,
}

func (process *TeleportProcess) initApps() {
	// If no applications are specified, exit early. This is due to the strange
	// behavior in reading file configuration. If the user does not specify an
	// "app_service" section, that is considered enabling "app_service".
	if len(process.Config.Apps.Apps) == 0 &&
		!process.Config.Apps.DebugApp &&
		!process.Config.Apps.MCPDemoServer &&
		len(process.Config.Apps.ResourceMatchers) == 0 {
		return
	}

	// Connect to the Auth Server, a client connected to the Auth Server will
	// be returned. For this to be successful, credentials to connect to the
	// Auth Server need to exist on disk or a registration token should be
	// provided.
	process.RegisterWithAuthServer(types.RoleApp, AppsIdentityEvent)

	// Define logger to prefix log lines with the name of the component and PID.
	component := teleport.Component(teleport.ComponentApp, process.id)
	logger := process.logger.With(teleport.ComponentKey, component)

	process.RegisterCriticalFunc("apps.start", func() error {
		conn, err := process.WaitForConnector(AppsIdentityEvent, logger)
		if conn == nil {
			return trace.Wrap(err)
		}

		shouldSkipCleanup := false
		defer func() {
			if !shouldSkipCleanup {
				warnOnErr(process.ExitContext(), conn.Close(), logger)
			}
		}()

		// Create a caching client to the Auth Server. It is to reduce load on
		// the Auth Server.
		accessPoint, err := process.newLocalCacheForApps(conn.Client, []string{component})
		if err != nil {
			return trace.Wrap(err)
		}
		resp, err := accessPoint.GetClusterNetworkingConfig(process.ExitContext())
		if err != nil {
			return trace.Wrap(err)
		}

		// If this process connected through the web proxy, it will discover the
		// reverse tunnel address correctly and store it in the connector.
		//
		// If it was not, it is running in single process mode which is used for
		// development and demos. In that case, wait until all dependencies (like
		// auth and reverse tunnel server) are ready before starting.
		tunnelAddrResolver := conn.TunnelProxyResolver()
		if tunnelAddrResolver == nil {
			tunnelAddrResolver = process.SingleProcessModeResolver(resp.GetProxyListenerMode())

			// run the resolver. this will check configuration for errors.
			_, _, err := tunnelAddrResolver(process.ExitContext())
			if err != nil {
				return trace.Wrap(err)
			}

			// Block and wait for all dependencies to start before starting.
			logger.DebugContext(process.ExitContext(), "Waiting for application service dependencies to start.")
			process.waitForAppDepend()
			logger.DebugContext(process.ExitContext(), "Application service dependencies have started, continuing.")
		}

		clusterName := conn.ClusterName()

		// Start header dumping debugging application if requested.
		if process.Config.Apps.DebugApp {
			process.initDebugApp()

			// Block until the header dumper application is ready, and once it is,
			// figure out where it's running and add it to the list of applications.
			event, err := process.WaitForEvent(process.ExitContext(), DebugAppReady)
			if err != nil {
				return trace.Wrap(err)
			}
			server, ok := event.Payload.(*httptest.Server)
			if !ok {
				return trace.BadParameter("unexpected payload %T", event.Payload)
			}
			process.Config.Apps.Apps = append(process.Config.Apps.Apps, servicecfg.App{
				Name: "dumper",
				URI:  server.URL,
			})
		}

		// Loop over each application and create a server.
		var applications types.Apps
		for _, app := range process.Config.Apps.Apps {
			publicAddr, err := getPublicAddr(accessPoint, app)
			if err != nil {
				return trace.Wrap(err)
			}

			var rewrite *types.Rewrite
			if app.Rewrite != nil {
				rewrite = &types.Rewrite{
					Redirect:  app.Rewrite.Redirect,
					JWTClaims: app.Rewrite.JWTClaims,
				}
				for _, header := range app.Rewrite.Headers {
					rewrite.Headers = append(rewrite.Headers,
						&types.Header{
							Name:  header.Name,
							Value: header.Value,
						})
				}
			}

			var aws *types.AppAWS
			if app.AWS != nil {
				aws = &types.AppAWS{
					ExternalID: app.AWS.ExternalID,
				}
			}

			a, err := types.NewAppV3(types.Metadata{
				Name:        app.Name,
				Description: app.Description,
				Labels:      app.StaticLabels,
			}, types.AppSpecV3{
				URI:                   app.URI,
				PublicAddr:            publicAddr,
				DynamicLabels:         types.LabelsToV2(app.DynamicLabels),
				InsecureSkipVerify:    app.InsecureSkipVerify,
				Rewrite:               rewrite,
				AWS:                   aws,
				Cloud:                 app.Cloud,
				RequiredAppNames:      app.RequiredAppNames,
				UseAnyProxyPublicAddr: app.UseAnyProxyPublicAddr,
				CORS:                  makeApplicationCORS(app.CORS),
				TCPPorts:              makeApplicationTCPPorts(app.TCPPorts),
				MCP:                   app.MCP,
			})
			if err != nil {
				return trace.Wrap(err)
			}

			applications = append(applications, a)
		}

		if process.Config.Apps.MCPDemoServer {
			if mcpDemoServer, err := mcp.NewDemoServerApp(); err != nil {
				logger.ErrorContext(process.ExitContext(), "Failed to create MCP demo server app")
			} else {
				applications = append(applications, mcpDemoServer)
			}
		}

		lockWatcher, err := services.NewLockWatcher(process.ExitContext(), services.LockWatcherConfig{
			ResourceWatcherConfig: services.ResourceWatcherConfig{
				Component: teleport.ComponentApp,
				Logger:    process.logger.With(teleport.ComponentKey, component),
				Client:    conn.Client,
			},
		})
		if err != nil {
			return trace.Wrap(err)
		}
		authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
			ClusterName: clusterName,
			AccessPoint: accessPoint,
			LockWatcher: lockWatcher,
			Logger:      process.logger.With(teleport.ComponentKey, component),
			DeviceAuthorization: authz.DeviceAuthorizationOpts{
				// Ignore the global device_trust.mode toggle, but allow role-based
				// settings to be applied.
				DisableGlobalMode: true,
			},
			PermitCaching: process.Config.CachePolicy.Enabled,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		tlsConfig, err := conn.ServerTLSConfig(process.Config.CipherSuites)
		if err != nil {
			return trace.Wrap(err)
		}

		asyncEmitter, err := process.NewAsyncEmitter(conn.Client)
		if err != nil {
			return trace.Wrap(err)
		}
		defer func() {
			if !shouldSkipCleanup {
				warnOnErr(process.ExitContext(), asyncEmitter.Close(), logger)
			}
		}()

		proxyGetter := reversetunnel.NewConnectedProxyGetter()

		connMonitor, err := srv.NewConnectionMonitor(srv.ConnectionMonitorConfig{
			AccessPoint:         accessPoint,
			LockWatcher:         lockWatcher,
			Clock:               process.Config.Clock,
			ServerID:            process.Config.HostUUID,
			Emitter:             asyncEmitter,
			EmitterContext:      process.ExitContext(),
			Logger:              process.logger,
			MonitorCloseChannel: process.Config.Apps.MonitorCloseChannel,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		connectionsHandler, err := app.NewConnectionsHandler(process.ExitContext(), &app.ConnectionsHandlerConfig{
			Clock:             process.Config.Clock,
			DataDir:           process.Config.DataDir,
			AuthClient:        conn.Client,
			AccessPoint:       accessPoint,
			Authorizer:        authorizer,
			TLSConfig:         tlsConfig,
			CipherSuites:      process.Config.CipherSuites,
			HostID:            process.Config.HostUUID,
			Emitter:           asyncEmitter,
			ConnectionMonitor: connMonitor,
			ServiceComponent:  teleport.ComponentApp,
			Logger:            logger,
			MCPDemoServer:     process.Config.Apps.MCPDemoServer,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		appServer, err := app.New(process.ExitContext(), &app.Config{
			Clock:                process.Config.Clock,
			AuthClient:           conn.Client,
			AccessPoint:          accessPoint,
			HostID:               process.Config.HostUUID,
			Hostname:             process.Config.Hostname,
			GetRotation:          process.GetRotation,
			Apps:                 applications,
			CloudLabels:          process.cloudLabels,
			ResourceMatchers:     process.Config.Apps.ResourceMatchers,
			OnHeartbeat:          process.OnHeartbeat(teleport.ComponentApp),
			ConnectedProxyGetter: proxyGetter,
			ConnectionsHandler:   connectionsHandler,
			InventoryHandle:      process.inventoryHandle,
		})
		if err != nil {
			return trace.Wrap(err)
		}

		defer func() {
			if !shouldSkipCleanup {
				warnOnErr(process.ExitContext(), appServer.Close(), logger)
			}
		}()

		// Start the apps server. This starts the server, heartbeat (services.App),
		// and (dynamic) label update.
		if err := appServer.Start(process.ExitContext()); err != nil {
			return trace.Wrap(err)
		}

		// Create and start an agent pool.
		agentPool, err := reversetunnel.NewAgentPool(
			process.ExitContext(),
			reversetunnel.AgentPoolConfig{
				Component:            teleport.ComponentApp,
				HostUUID:             conn.HostID(),
				Resolver:             tunnelAddrResolver,
				Client:               conn.Client,
				Server:               appServer,
				AccessPoint:          accessPoint,
				AuthMethods:          conn.ClientAuthMethods(),
				Cluster:              clusterName,
				FIPS:                 process.Config.FIPS,
				ConnectedProxyGetter: proxyGetter,
			})
		if err != nil {
			return trace.Wrap(err)
		}
		err = agentPool.Start()
		if err != nil {
			return trace.Wrap(err)
		}

		process.BroadcastEvent(Event{Name: AppsReady, Payload: nil})
		logger.InfoContext(process.ExitContext(), "All applications successfully started.")

		// Cancel deferred cleanup actions, because we're going
		// to register an OnExit handler to take care of it
		shouldSkipCleanup = true

		// Execute this when process is asked to exit.
		process.OnExit("apps.stop", func(payload any) {
			if payload == nil {
				logger.InfoContext(process.ExitContext(), "Shutting down immediately.")
				warnOnErr(process.ExitContext(), appServer.Close(), logger)
			} else {
				logger.InfoContext(process.ExitContext(), "Shutting down gracefully.")
				warnOnErr(process.ExitContext(), appServer.Shutdown(payloadContext(payload)), logger)
			}
			if asyncEmitter != nil {
				warnOnErr(process.ExitContext(), asyncEmitter.Close(), logger)
			}
			agentPool.Stop()
			warnOnErr(process.ExitContext(), asyncEmitter.Close(), logger)
			warnOnErr(process.ExitContext(), conn.Close(), logger)
			logger.InfoContext(process.ExitContext(), "Exited.")
		})

		// Block and wait while the server and agent pool are running.
		if err := appServer.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			return trace.Wrap(err)
		}
		agentPool.Wait()
		return nil
	})
}

func warnOnErr(ctx context.Context, err error, log *slog.Logger) {
	if err != nil {
		// don't warn on double close, happens sometimes when
		// calling accept on a closed listener
		if utils.IsOKNetworkError(err) {
			return
		}
		log.WarnContext(ctx, "Got error while cleaning up.", "error", err)
	}
}

// initAuthStorage initializes the storage backend for the auth service.
func (process *TeleportProcess) initAuthStorage() (backend.Backend, error) {
	ctx := context.TODO()
	bc := process.Config.Auth.StorageConfig
	process.logger.DebugContext(process.ExitContext(), "Initializing auth backend.", "type", bc.Type)
	bk, err := backend.New(ctx, bc.Type, bc.Params)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	reporter, err := backend.NewReporter(backend.ReporterConfig{
		Component:  teleport.ComponentBackend,
		Backend:    backend.NewSanitizer(bk),
		Tracer:     process.TracingProvider.Tracer(teleport.ComponentBackend),
		Registerer: process.metricsRegistry,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.setReporter(reporter)
	return reporter, nil
}

func (process *TeleportProcess) setReporter(reporter *backend.Reporter) {
	process.Lock()
	defer process.Unlock()
	process.reporter = reporter
}

// WaitWithContext waits until all internal services stop.
func (process *TeleportProcess) WaitWithContext(ctx context.Context) {
	local, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		if err := process.Supervisor.Wait(); err != nil {
			process.logger.WarnContext(process.ExitContext(), "Error waiting for all services to complete", "error", err)
		}
	}()

	<-local.Done()
}

// StartShutdown launches non-blocking graceful shutdown process that signals
// completion, returns context that will be closed once the shutdown is done
func (process *TeleportProcess) StartShutdown(ctx context.Context) context.Context {
	shutdownDelayTimer := process.Clock.NewTimer(process.Config.ShutdownDelay)
	defer shutdownDelayTimer.Stop()

	hasChildren := process.forkedTeleportCount.Load() > 0
	if hasChildren {
		ctx = services.ProcessForkedContext(ctx)
	}

	process.BroadcastEvent(Event{Name: TeleportTerminatingEvent})

	if process.inventoryHandle != nil {
		deleteResources := !hasChildren
		if err := process.inventoryHandle.SetAndSendGoodbye(ctx, deleteResources, hasChildren); err != nil {
			process.logger.WarnContext(process.ExitContext(), "Failed sending inventory goodbye during shutdown", "error", err)
		}
	}

	if d := process.Config.ShutdownDelay; d > 0 {
		if hasChildren {
			process.logger.InfoContext(ctx, "Ignoring shutdown delay due to the presence of forked processes")
		} else {
			process.logger.InfoContext(ctx, "Waiting for shutdown delay", "shutdown_delay", d.String())
			select {
			case <-shutdownDelayTimer.Chan():
			case <-process.ExitContext().Done():
				process.logger.WarnContext(ctx, "Skipping shutdown delay early due to process exit")
			case <-ctx.Done():
				process.logger.WarnContext(ctx, "Skipping shutdown delay early due to context cancellation")
			}
		}
	}

	// by the time we get here we've already extracted the parent pipe, which is
	// the only potential imported file descriptor that's not a listening
	// socket, so closing every imported FD with a prefix of "" will close all
	// imported listeners that haven't been used so far
	warnOnErr(process.ExitContext(), process.closeImportedDescriptors(""), process.logger)
	warnOnErr(process.ExitContext(), process.stopListeners(), process.logger)

	process.BroadcastEvent(Event{Name: TeleportExitEvent, Payload: ctx})
	localCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		if err := process.Supervisor.Wait(); err != nil {
			process.logger.WarnContext(process.ExitContext(), "Error waiting for all services to complete", "error", err)
		}
		process.logger.DebugContext(process.ExitContext(), "All supervisor functions are completed.")

		if localAuth := process.getLocalAuth(); localAuth != nil {
			if err := localAuth.Close(); err != nil {
				process.logger.WarnContext(process.ExitContext(), "Failed closing auth server.", "error", err)
			}
		}

		if process.storage != nil {
			if err := process.storage.Close(); err != nil {
				process.logger.WarnContext(process.ExitContext(), "Failed closing process storage.", "error", err)
			}
		}

		if process.inventoryHandle != nil {
			process.inventoryHandle.Close()
		}
	}()
	go process.printShutdownStatus(localCtx)
	return localCtx
}

// Shutdown launches graceful shutdown process and waits
// for it to complete
func (process *TeleportProcess) Shutdown(ctx context.Context) {
	localCtx := process.StartShutdown(ctx)
	// wait until parent context closes
	<-localCtx.Done()
	process.logger.DebugContext(ctx, "Process completed.")
}

// Close broadcasts close signals and exits immediately
func (process *TeleportProcess) Close() error {
	// generate a TeleportTerminatingEvent to unblock any service waiting on
	// that event before TeleportExitEvent
	process.BroadcastEvent(Event{Name: TeleportTerminatingEvent})
	process.BroadcastEvent(Event{Name: TeleportExitEvent})

	var errors []error

	if localAuth := process.getLocalAuth(); localAuth != nil {
		errors = append(errors, localAuth.Close())
	}

	if process.storage != nil {
		errors = append(errors, process.storage.Close())
	}

	if process.inventoryHandle != nil {
		process.inventoryHandle.Close()
	}

	return trace.NewAggregate(errors...)
}

// initSelfSignedHTTPSCert generates and self-signs a TLS key+cert pair for https connection
// to the proxy server.
func initSelfSignedHTTPSCert(cfg *servicecfg.Config) (err error) {
	ctx := context.Background()
	cfg.Logger.WarnContext(ctx, "No TLS Keys provided, using self-signed certificate.")

	keyPath := filepath.Join(cfg.DataDir, defaults.SelfSignedKeyPath)
	certPath := filepath.Join(cfg.DataDir, defaults.SelfSignedCertPath)

	cfg.Proxy.KeyPairs = append(cfg.Proxy.KeyPairs, servicecfg.KeyPairPath{
		PrivateKey:  keyPath,
		Certificate: certPath,
	})

	// return the existing pair if they have already been generated:
	_, err = tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return trace.Wrap(err, "unrecognized error reading certs")
	}
	cfg.Logger.WarnContext(ctx, "Generating self-signed key and cert.", "key_path", keyPath, "cert_path", certPath)

	hosts := []string{cfg.Hostname, "localhost"}
	var ips []string

	// add web public address hosts to self-signed cert
	for _, addr := range cfg.Proxy.PublicAddrs {
		proxyHost, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			// log and skip error since this is a nice to have
			cfg.Logger.WarnContext(ctx, "Error parsing proxy.public_address, skipping adding to self-signed cert", "public_address", addr.String(), "error", err)
			continue
		}
		// If the address is a IP have it added as IP SAN
		if ip := net.ParseIP(proxyHost); ip != nil {
			ips = append(ips, proxyHost)
		} else {
			hosts = append(hosts, proxyHost)
		}
	}

	creds, err := cert.GenerateSelfSignedCert(hosts, ips)
	if err != nil {
		return trace.Wrap(err)
	}

	if err := os.WriteFile(keyPath, creds.PrivateKey, 0o600); err != nil {
		return trace.Wrap(err, "error writing key PEM")
	}
	if err := os.WriteFile(certPath, creds.Cert, 0o600); err != nil {
		return trace.Wrap(err, "error writing key PEM")
	}
	return nil
}

// initDebugApp starts a debug server that dumpers request headers.
func (process *TeleportProcess) initDebugApp() {
	process.RegisterFunc("debug.app.service", func() error {
		server := httptest.NewServer(http.HandlerFunc(dumperHandler))
		process.BroadcastEvent(Event{Name: DebugAppReady, Payload: server})

		process.OnExit("debug.app.shutdown", func(payload any) {
			server.Close()
			process.logger.InfoContext(process.ExitContext(), "Exited.")
		})
		return nil
	})
}

// SingleProcessModeResolver returns the reversetunnel.Resolver that should be used when running all components needed
// within the same process. It's used for development and demo purposes.
func (process *TeleportProcess) SingleProcessModeResolver(mode types.ProxyListenerMode) reversetunnelclient.Resolver {
	return func(context.Context) (*utils.NetAddr, types.ProxyListenerMode, error) {
		addr, ok := process.singleProcessMode(mode)
		if !ok {
			return nil, mode, trace.BadParameter(`failed to find reverse tunnel address, if running in single process mode, make sure "auth_service", "proxy_service", and "app_service" are all enabled`)
		}
		return addr, mode, nil
	}
}

// singleProcessMode returns true when running all components needed within
// the same process. It's used for development and demo purposes.
func (process *TeleportProcess) singleProcessMode(mode types.ProxyListenerMode) (*utils.NetAddr, bool) {
	if !process.Config.Proxy.Enabled || !process.Config.Auth.Enabled {
		return nil, false
	}
	if process.Config.Proxy.DisableReverseTunnel {
		return nil, false
	}

	if !process.Config.Proxy.DisableTLS && !process.Config.Proxy.DisableALPNSNIListener && mode == types.ProxyListenerMode_Multiplex {
		var addr utils.NetAddr
		switch {
		// Use the public address if available.
		case len(process.Config.Proxy.PublicAddrs) != 0:
			addr = process.Config.Proxy.PublicAddrs[0]

		// If WebAddress is unspecified "0.0.0.0" replace 0.0.0.0 with localhost since 0.0.0.0 is never a valid
		// principal (auth server explicitly removes it when issuing host certs) and when WebPort is used
		// in the single process mode to establish SSH reverse tunnel connection the host is validated against
		// the valid principal list.
		default:
			addr = process.Config.Proxy.WebAddr
			addr.Addr = utils.ReplaceUnspecifiedHost(&addr, defaults.HTTPListenPort)
		}

		// In case the address has "https" scheme for TLS Routing, make sure
		// "tcp" is used when dialing reverse tunnel.
		if addr.AddrNetwork == "https" {
			addr.AddrNetwork = "tcp"
		}
		return &addr, true
	}

	if len(process.Config.Proxy.TunnelPublicAddrs) == 0 {
		addr, err := utils.ParseHostPortAddr(string(teleport.PrincipalLocalhost), defaults.SSHProxyTunnelListenPort)
		if err != nil {
			return nil, false
		}
		return addr, true
	}
	return &process.Config.Proxy.TunnelPublicAddrs[0], true
}

// dumperHandler is an Application Access debugging application that will
// dump the headers of a request.
func dumperHandler(w http.ResponseWriter, r *http.Request) {
	requestDump, err := httputil.DumpRequest(r, true)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	randomBytes := make([]byte, 8)
	rand.Reader.Read(randomBytes)
	cookieValue := hex.EncodeToString(randomBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     "dumper.session.cookie",
		Value:    cookieValue,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, string(requestDump))
}

// getPublicAddr waits for a proxy to be registered with Teleport.
func getPublicAddr(authClient authclient.ReadAppsAccessPoint, a servicecfg.App) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case <-ticker.C:
			publicAddr, err := app.FindPublicAddr(authClient, a.PublicAddr, a.Name)

			if err == nil {
				return publicAddr, nil
			}
		case <-timeout.C:
			return "", trace.BadParameter("timed out waiting for proxy with public address")
		}
	}
}

// newHTTPFileSystem creates a new HTTP file system for the web handler.
// It uses external configuration to make the decision
func newHTTPFileSystem() (http.FileSystem, error) {
	fs, err := teleport.NewWebAssetsFilesystem() //nolint:staticcheck // linter fails on non-linux system as only linux implementation returns useful values.
	if err != nil {                              //nolint:staticcheck // linter fails on non-linux system as only linux implementation returns useful values.
		return nil, trace.Wrap(err)
	}
	return fs, nil
}

// readOrGenerateHostID tries to read the `host_uuid` from Kubernetes storage (if available) or local storage.
// If the read operation returns no `host_uuid`, this function tries to pick it from the first static identity provided.
// If no static identities were defined for the process, a new id is generated depending on the joining process:
// - types.JoinMethodEC2: we will use the EC2 NodeID: {accountID}-{nodeID}
// - Any other valid Joining method: a new UUID is generated.
// Finally, if a new id is generated, this function writes it into local storage and Kubernetes storage (if available).
// If kubeBackend is nil, the agent is not running in a Kubernetes Cluster.
func readOrGenerateHostID(ctx context.Context, cfg *servicecfg.Config, kubeBackend kubernetesBackend) (err error) {
	// Load `host_uuid` from different storages. If this process is running in a Kubernetes Cluster,
	// readHostUUIDFromStorages will try to read the `host_uuid` from the Kubernetes Secret. If the
	// key is empty or if not running in a Kubernetes Cluster, it will read the
	// `host_uuid` from local data directory.
	cfg.HostUUID, err = readHostIDFromStorages(ctx, cfg.DataDir, kubeBackend)
	if err != nil {
		if !trace.IsNotFound(err) {
			if errors.Is(err, fs.ErrPermission) {
				cfg.Logger.ErrorContext(ctx, "Teleport does not have permission to write to the data directory. Ensure that you are running as a user with appropriate permissions.", "data_dir", cfg.DataDir)
			}
			return trace.Wrap(err)
		}
		// if there's no host uuid initialized yet, try to read one from the
		// one of the identities
		if len(cfg.Identities) != 0 {
			cfg.HostUUID = cfg.Identities[0].ID.HostUUID
			cfg.Logger.InfoContext(ctx, "Taking host UUID from first identity.", "host_uuid", cfg.HostUUID)
		} else {
			switch cfg.JoinMethod {
			case types.JoinMethodToken,
				types.JoinMethodUnspecified,
				types.JoinMethodAzureDevops,
				types.JoinMethodIAM,
				types.JoinMethodCircleCI,
				types.JoinMethodKubernetes,
				types.JoinMethodGitHub,
				types.JoinMethodGitLab,
				types.JoinMethodAzure,
				types.JoinMethodGCP,
				types.JoinMethodTPM,
				types.JoinMethodTerraformCloud,
				types.JoinMethodOracle:
				// Checking error instead of the usual uuid.New() in case uuid generation
				// fails due to not enough randomness. It's been known to happen happen when
				// Teleport starts very early in the node initialization cycle and /dev/urandom
				// isn't ready yet.
				rawID, err := uuid.NewRandom()
				if err != nil {
					return trace.BadParameter("" +
						"Teleport failed to generate host UUID. " +
						"This may happen if randomness source is not fully initialized when the node is starting up. " +
						"Please try restarting Teleport again.")
				}
				cfg.HostUUID = rawID.String()
			case types.JoinMethodEC2:
				cfg.HostUUID, err = utils.GetEC2NodeID(ctx)
				if err != nil {
					return trace.Wrap(err)
				}
			default:
				return trace.BadParameter("unknown join method %q", cfg.JoinMethod)
			}
			cfg.Logger.InfoContext(ctx, "Generating new host UUID", "host_uuid", cfg.HostUUID)
		}
		// persistHostUUIDToStorages will persist the host_uuid to the local storage
		// and to Kubernetes Secret if this process is running on a Kubernetes Cluster.
		if err := persistHostIDToStorages(ctx, cfg, kubeBackend); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

// kubernetesBackend interface for kube storage backend.
type kubernetesBackend interface {
	// Put puts value into backend (creates if it does not
	// exists, updates it otherwise)
	Put(ctx context.Context, i backend.Item) (*backend.Lease, error)
	// Get returns a single item or not found error
	Get(ctx context.Context, key backend.Key) (*backend.Item, error)
}

// readHostIDFromStorages tries to read the `host_uuid` value from different storages,
// depending on where the process is running.
// If the process is running in a Kubernetes Cluster, this function will attempt
// to read the `host_uuid` from the Kubernetes Secret. If it does not exist or
// if it is not running on a Kubernetes cluster the read is done from the local
// storage: `dataDir/host_uuid`.
func readHostIDFromStorages(ctx context.Context, dataDir string, kubeBackend kubernetesBackend) (string, error) {
	if kubeBackend != nil {
		if hostID, err := loadHostIDFromKubeSecret(ctx, kubeBackend); err == nil && len(hostID) > 0 {
			return hostID, nil
		}
	}
	// Even if running in Kubernetes fallback to local storage if `host_uuid` was
	// not found in secret.
	hostID, err := hostid.ReadFile(dataDir)
	return hostID, trace.Wrap(err)
}

// persistHostIDToStorages writes the cfg.HostUUID to local data and to
// Kubernetes Secret if this process is running on a Kubernetes Cluster.
func persistHostIDToStorages(ctx context.Context, cfg *servicecfg.Config, kubeBackend kubernetesBackend) error {
	if err := hostid.WriteFile(cfg.DataDir, cfg.HostUUID); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			cfg.Logger.ErrorContext(ctx, "Teleport does not have permission to write to the data directory. Ensure that you are running as a user with appropriate permissions.", "data_dir", cfg.DataDir)
		}
		return trace.Wrap(err)
	}

	// Persists the `host_uuid` into Kubernetes Secret for later reusage.
	// This is required because `host_uuid` is part of the client secret
	// and Auth connection will fail if we present a different `host_uuid`.
	if kubeBackend != nil {
		return trace.Wrap(writeHostIDToKubeSecret(ctx, kubeBackend, cfg.HostUUID))
	}
	return nil
}

// loadHostIDFromKubeSecret reads the host_uuid from the Kubernetes secret with
// the expected key: `/host_uuid`.
func loadHostIDFromKubeSecret(ctx context.Context, kubeBackend kubernetesBackend) (string, error) {
	item, err := kubeBackend.Get(ctx, backend.NewKey(hostid.FileName))
	if err != nil {
		return "", trace.Wrap(err)
	}
	return string(item.Value), nil
}

// writeHostIDToKubeSecret writes the `host_uuid` into the Kubernetes secret under
// the key `/host_uuid`.
func writeHostIDToKubeSecret(ctx context.Context, kubeBackend kubernetesBackend, id string) error {
	_, err := kubeBackend.Put(
		ctx,
		backend.Item{
			Key:   backend.NewKey(hostid.FileName),
			Value: []byte(id),
		},
	)
	return trace.Wrap(err)
}

// initPublicGRPCServer creates and registers a gRPC server that does not use client
// certificates for authentication. This is used by the join service, which nodes
// use to receive a signed certificate from the auth server.
func (process *TeleportProcess) initPublicGRPCServer(
	limiter *limiter.Limiter,
	conn *Connector,
	listener net.Listener,
) (*grpc.Server, error) {
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			interceptors.GRPCServerUnaryErrorInterceptor,
			limiter.UnaryServerInterceptor(),
		),
		grpc.ChainStreamInterceptor(
			interceptors.GRPCServerStreamErrorInterceptor,
			limiter.StreamServerInterceptor,
		),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Using an aggressive idle timeout here since this gRPC server
			// currently only hosts the join service, which has no need for
			// long-lived idle connections.
			//
			// The reason for introducing this is that teleport clients
			// before #17685 is fixed will hold connections open
			// indefinitely if they encounter an error during the joining
			// process, and this seems like the best way for the server to
			// forcibly close those connections.
			//
			// If another gRPC service is added here in the future, it
			// should be alright to increase or remove this idle timeout as
			// necessary once the client fix has been released and widely
			// available for some time.
			MaxConnectionIdle: 10 * time.Second,
		}),
		grpc.MaxConcurrentStreams(defaults.GRPCMaxConcurrentStreams),
	)
	joinServiceServer := joinserver.NewJoinServiceGRPCServer(conn.Client)
	proto.RegisterJoinServiceServer(server, joinServiceServer)

	accessGraphProxySvc, err := secretsscannerproxy.New(
		secretsscannerproxy.ServiceConfig{
			AuthClient: conn.Client,
			Log:        process.logger,
		})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	accessgraphsecretsv1pb.RegisterSecretsScannerServiceServer(server, accessGraphProxySvc)

	process.RegisterCriticalFunc("proxy.grpc.public", func() error {
		process.logger.InfoContext(process.ExitContext(), "Starting proxy gRPC server.", "listen_address", listener.Addr())
		return trace.Wrap(server.Serve(listener))
	})
	return server, nil
}

// initSecureGRPCServer creates and registers a gRPC server that uses mTLS for
// authentication. This is used for the gRPC Kube service, which allows users to
// safely access Kubernetes clusters resources via Teleport without leaking certificates.
// The gRPC server handles the mTLS because we require the client certificate to be
// subject in order to determine his identity.
func (process *TeleportProcess) initSecureGRPCServer(cfg initSecureGRPCServerCfg) (*grpc.Server, error) {
	if !process.Config.Proxy.Kube.Enabled {
		return nil, nil
	}
	clusterName := cfg.conn.ClusterName()
	serverTLSConfig, err := cfg.conn.ServerTLSConfig(process.Config.CipherSuites)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	authorizer, err := authz.NewAuthorizer(authz.AuthorizerOpts{
		ClusterName:   clusterName,
		AccessPoint:   cfg.accessPoint,
		LockWatcher:   cfg.lockWatcher,
		Logger:        process.logger.With(teleport.ComponentKey, teleport.Component(teleport.ComponentProxySecureGRPC, process.id)),
		PermitCaching: process.Config.CachePolicy.Enabled,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// authMiddleware authenticates request assuming TLS client authentication
	// adds authentication information to the context
	// and passes it to the API server
	authMiddleware := &auth.Middleware{
		Middleware: authz.Middleware{
			ClusterName:   clusterName,
			AcceptedUsage: []string{teleport.UsageKubeOnly},
		},
		Limiter: cfg.limiter,
	}

	tlsConf := serverTLSConfig.Clone()
	tlsConf.NextProtos = []string{string(alpncommon.ProtocolHTTP2), string(alpncommon.ProtocolProxyGRPCSecure)}
	tlsConf = copyAndConfigureTLS(tlsConf, process.logger, cfg.accessPoint, clusterName)
	creds, err := auth.NewTransportCredentials(auth.TransportCredentialsConfig{
		TransportCredentials: credentials.NewTLS(tlsConf),
		UserGetter:           authMiddleware,
		GetAuthPreference:    cfg.accessPoint.GetAuthPreference,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authMiddleware.UnaryInterceptors()...),
		grpc.ChainStreamInterceptor(authMiddleware.StreamInterceptors()...),
		grpc.Creds(creds),
		grpc.MaxConcurrentStreams(defaults.GRPCMaxConcurrentStreams),
	)

	kubeServer, err := kubegrpc.New(kubegrpc.Config{
		AccessPoint:           cfg.accessPoint,
		Authz:                 authorizer,
		Log:                   process.logger,
		Emitter:               cfg.emitter,
		KubeProxyAddr:         cfg.kubeProxyAddr.String(),
		ClusterName:           clusterName,
		GetConnTLSCertificate: cfg.conn.ClientGetCertificate,
		GetConnTLSRoots:       cfg.conn.ClientGetPool,
		ConnTLSCipherSuites:   cfg.tlsCipherSuites,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	kubeproto.RegisterKubeServiceServer(server, kubeServer)

	process.RegisterCriticalFunc("proxy.grpc.secure", func() error {
		process.logger.InfoContext(process.ExitContext(), "Starting proxy gRPC server.", "listen_address", cfg.listener.Addr())
		return trace.Wrap(server.Serve(cfg.listener))
	})
	return server, nil
}

// initSecureGRPCServerCfg is a configuration for initSecureGRPCServer function.
type initSecureGRPCServerCfg struct {
	conn            *Connector
	limiter         *limiter.Limiter
	listener        net.Listener
	kubeProxyAddr   utils.NetAddr
	accessPoint     authclient.ProxyAccessPoint
	lockWatcher     *services.LockWatcher
	emitter         apievents.Emitter
	tlsCipherSuites []uint16
}

// copyAndConfigureTLS can be used to copy and modify an existing *tls.Config
// for Teleport application proxy servers.
func copyAndConfigureTLS(config *tls.Config, log *slog.Logger, accessPoint authclient.AccessCache, clusterName string) *tls.Config {
	tlsConfig := config.Clone()

	// Require clients to present a certificate
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert

	// Configure function that will be used to fetch the CA that signed the
	// client's certificate to verify the chain presented. If the client does not
	// pass in the cluster name, this functions pulls back all CA to try and
	// match the certificate presented against any CA.
	tlsConfig.GetConfigForClient = authclient.WithClusterCAs(tlsConfig.Clone(), accessPoint, clusterName, log)

	return tlsConfig
}

func makeXForwardedForMiddleware(cfg *servicecfg.Config) utils.HTTPMiddleware {
	if cfg.Proxy.TrustXForwardedFor {
		return web.NewXForwardedForMiddleware
	}
	return utils.NoopHTTPMiddleware
}

// makeApplicationCORS converts a servicecfg.CORS to a types.CORS.
func makeApplicationCORS(c *servicecfg.CORS) *types.CORSPolicy {
	if c == nil {
		return nil
	}

	return &types.CORSPolicy{
		AllowedOrigins:   c.AllowedOrigins,
		AllowedMethods:   c.AllowedMethods,
		AllowedHeaders:   c.AllowedHeaders,
		AllowCredentials: c.AllowCredentials,
		MaxAge:           uint32(c.MaxAge),
	}
}

// makeApplicationTCPPorts converts a slice of [servicecfg.PortRange] to a slice of [types.PortRange].
func makeApplicationTCPPorts(servicePorts []servicecfg.PortRange) []*types.PortRange {
	ports := make([]*types.PortRange, 0, len(servicePorts))
	for _, portRange := range servicePorts {
		ports = append(ports, &types.PortRange{
			Port:    uint32(portRange.Port),
			EndPort: uint32(portRange.EndPort),
		})
	}
	return ports
}

func (process *TeleportProcess) newExternalAuditStorageConfigurator() (*externalauditstorage.Configurator, error) {
	watcher, err := local.NewClusterExternalAuditWatcher(process.GracefulExitContext(), local.ClusterExternalAuditStorageWatcherConfig{
		Backend: process.backend,
		OnChange: func() {
			// this is only reached in Cloud on the Auth Service instance; we
			// can't change the settings for athenaevents and s3sessions after
			// they've been initialized, and we can't swap in new audit log and
			// session recording backends, so instead we just hard close the
			// process (no need for any graceful shutdowns, as the Auth Service
			// doesn't really gracefully stop anything anyway) and let the
			// orchestration start us again
			process.Close()
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Wait for the watcher to init to avoid a race in case the external audit
	// storage config changes after the configurator loads it and before the
	// watcher initialized.
	watcher.WaitInit(process.GracefulExitContext())

	easSvc := local.NewExternalAuditStorageService(process.backend)
	integrationSvc, err := local.NewIntegrationsService(process.backend)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	statusService := local.NewStatusService(process.backend)
	return externalauditstorage.NewConfigurator(process.ExitContext(), easSvc, integrationSvc, statusService)
}

// createLockedPIDFile creates a PID file in the path specified by pidFile
// containing the current PID, atomically swapping it in the final place and
// leaving it with an exclusive advisory lock that will get released when the
// process ends, for the benefit of "pkill -L".
func createLockedPIDFile(pidFile string) error {
	pending, err := renameio.NewPendingFile(pidFile, renameio.WithPermissions(0o644))
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	defer pending.Cleanup()
	if _, err := fmt.Fprintf(pending, "%v\n", os.Getpid()); err != nil {
		return trace.ConvertSystemError(err)
	}

	const minimumDupFD = 3 // skip stdio
	locker, err := unix.FcntlInt(pending.Fd(), unix.F_DUPFD_CLOEXEC, minimumDupFD)
	runtime.KeepAlive(pending)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	if err := unix.Flock(locker, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(locker)
		return trace.ConvertSystemError(err)
	}
	// deliberately leak the fd to hold the lock until the process dies

	if err := pending.CloseAtomicallyReplace(); err != nil {
		return trace.ConvertSystemError(err)
	}
	return nil
}
