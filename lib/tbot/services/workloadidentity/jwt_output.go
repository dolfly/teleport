// Teleport
// Copyright (C) 2025 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workloadidentity

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/gravitational/trace"

	apiclient "github.com/gravitational/teleport/api/client"
	workloadidentityv1pb "github.com/gravitational/teleport/api/gen/proto/go/teleport/workloadidentity/v1"
	"github.com/gravitational/teleport/api/utils/retryutils"
	"github.com/gravitational/teleport/lib/tbot/bot"
	"github.com/gravitational/teleport/lib/tbot/client"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/gravitational/teleport/lib/tbot/internal"
	"github.com/gravitational/teleport/lib/tbot/readyz"
	"github.com/gravitational/teleport/lib/tbot/workloadidentity"
)

func JWTOutputServiceBuilder(
	cfg *JWTOutputConfig,
	trustBundleCache TrustBundleGetter,
	defaultCredentialLifetime bot.CredentialLifetime,
) bot.ServiceBuilder {
	return func(deps bot.ServiceDependencies) (bot.Service, error) {
		if err := cfg.CheckAndSetDefaults(); err != nil {
			return nil, trace.Wrap(err)
		}
		svc := &JWTOutputService{
			botAuthClient:             deps.Client,
			defaultCredentialLifetime: defaultCredentialLifetime,
			cfg:                       cfg,
			getBotIdentity:            deps.BotIdentity,
			identityGenerator:         deps.IdentityGenerator,
			clientBuilder:             deps.ClientBuilder,
		}
		svc.log = deps.LoggerForService(svc)
		svc.statusReporter = deps.StatusRegistry.AddService(svc.String())
		return svc, nil
	}
}

// JWTOutputService is a service that retrieves JWT workload identity
// credentials for WorkloadIdentity resources.
type JWTOutputService struct {
	botAuthClient             *apiclient.Client
	defaultCredentialLifetime bot.CredentialLifetime
	cfg                       *JWTOutputConfig
	getBotIdentity            func() *identity.Identity
	log                       *slog.Logger
	statusReporter            readyz.Reporter
	// trustBundleCache is the cache of trust bundles. It only needs to be
	// provided when running in daemon mode.
	trustBundleCache  TrustBundleGetter
	identityGenerator *identity.Generator
	clientBuilder     *client.Builder
}

// String returns a human-readable description of the service.
func (s *JWTOutputService) String() string {
	return cmp.Or(
		s.cfg.Name,
		fmt.Sprintf("workload-identity-jwt (%s)", s.cfg.Destination.String()),
	)
}

// OneShot runs the service once, generating the output and writing it to the
// destination, before exiting.
func (s *JWTOutputService) OneShot(ctx context.Context) error {
	res, err := s.requestJWTSVID(ctx)
	if err != nil {
		return trace.Wrap(err, "requesting JWT SVID")
	}
	return s.render(ctx, res)
}

// Run runs the service in daemon mode, periodically generating the output and
// writing it to the destination.
func (s *JWTOutputService) Run(ctx context.Context) error {
	bundleSet, err := s.trustBundleCache.GetBundleSet(ctx)
	if err != nil {
		return trace.Wrap(err, "getting trust bundle set")
	}

	jitter := retryutils.DefaultJitter
	var cred *workloadidentityv1pb.Credential
	var failures int
	firstRun := make(chan struct{}, 1)
	firstRun <- struct{}{}
	for {
		var retryAfter <-chan time.Time
		if failures > 0 {
			s.statusReporter.Report(readyz.Unhealthy)
			backoffTime := min(time.Second*time.Duration(math.Pow(2, float64(failures-1))), time.Minute)
			backoffTime = jitter(backoffTime)
			s.log.WarnContext(
				ctx,
				"Last attempt to generate output failed, will retry",
				"retry_after", backoffTime,
				"failures", failures,
			)
			retryAfter = time.After(time.Duration(failures) * time.Second)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-retryAfter:
			s.log.InfoContext(ctx, "Retrying")
		case <-bundleSet.Stale():
			// We don't actually write this bundle out at the moment, but, we
			// still track it so we know when to reissue the JWT SVID.
			newBundleSet, err := s.trustBundleCache.GetBundleSet(ctx)
			if err != nil {
				return trace.Wrap(err, "getting trust bundle set")
			}
			s.log.InfoContext(ctx, "Trust bundle set has been updated")
			if !newBundleSet.Local.Equal(bundleSet.Local) {
				// If the local trust domain CA has changed, we need to reissue
				// the SVID.
				cred = nil
			}
			bundleSet = newBundleSet
		case <-time.After(cmp.Or(s.cfg.CredentialLifetime, s.defaultCredentialLifetime).RenewalInterval):
			s.log.InfoContext(ctx, "Renewal interval reached, renewing SVIDs")
			cred = nil
		case <-firstRun:
		}

		if cred == nil {
			var err error
			cred, err = s.requestJWTSVID(ctx)
			if err != nil {
				s.log.ErrorContext(ctx, "Failed to request JWT SVID", "error", err)
				failures++
				continue
			}
		}
		if err := s.render(ctx, cred); err != nil {
			s.log.ErrorContext(ctx, "Failed to render output", "error", err)
			failures++
			continue
		}
		s.statusReporter.Report(readyz.Healthy)
		failures = 0
	}
}

func (s *JWTOutputService) requestJWTSVID(
	ctx context.Context,
) (
	*workloadidentityv1pb.Credential,
	error,
) {
	ctx, span := tracer.Start(
		ctx,
		"JWTOutputService/requestJWTSVID",
	)
	defer span.End()

	effectiveLifetime := cmp.Or(s.cfg.CredentialLifetime, s.defaultCredentialLifetime)
	id, err := s.identityGenerator.GenerateFacade(ctx,
		identity.WithLifetime(effectiveLifetime.TTL, effectiveLifetime.RenewalInterval),
		identity.WithLogger(s.log),
	)
	if err != nil {
		return nil, trace.Wrap(err, "generating identity")
	}
	// create a client that uses the impersonated identity, so that when we
	// fetch information, we can ensure access rights are enforced.
	impersonatedClient, err := s.clientBuilder.Build(ctx, id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer impersonatedClient.Close()

	credentials, err := workloadidentity.IssueJWTWorkloadIdentity(
		ctx,
		s.log,
		impersonatedClient,
		s.cfg.Selector,
		s.cfg.Audiences,
		effectiveLifetime.TTL,
		nil,
	)
	if err != nil {
		return nil, trace.Wrap(err, "generating JWT SVID")
	}

	var credential *workloadidentityv1pb.Credential
	switch len(credentials) {
	case 0:
		return nil, trace.BadParameter("no JWT SVIDs returned")
	case 1:
		credential = credentials[0]
	default:
		// We could eventually implement some kind of hint selection mechanism
		// to pick the "right" one.
		received := make([]string, 0, len(credentials))
		for _, cred := range credentials {
			received = append(received,
				fmt.Sprintf(
					"%s:%s",
					cred.WorkloadIdentityName,
					cred.SpiffeId,
				),
			)
		}
		return nil, trace.BadParameter(
			"multiple JWT SVIDs received: %v", received,
		)
	}

	return credential, nil
}

func (s *JWTOutputService) render(
	ctx context.Context,
	cred *workloadidentityv1pb.Credential,
) error {
	ctx, span := tracer.Start(
		ctx,
		"JWTOutputService/render",
	)
	defer span.End()
	s.log.InfoContext(ctx, "Rendering output")

	// Check the ACLs. We can't fix them, but we can warn if they're
	// misconfigured. We'll need to precompute a list of keys to check.
	// Note: This may only log a warning, depending on configuration.
	if err := s.cfg.Destination.Verify(identity.ListKeys(identity.DestinationKinds()...)); err != nil {
		return trace.Wrap(err)
	}
	// Ensure this destination is also writable. This is a hard fail if
	// ACLs are misconfigured, regardless of configuration.
	if err := identity.VerifyWrite(ctx, s.cfg.Destination); err != nil {
		return trace.Wrap(err, "verifying destination")
	}

	if err := s.cfg.Destination.Write(
		ctx, internal.JWTSVIDPath, []byte(cred.GetJwtSvid().GetJwt()),
	); err != nil {
		return trace.Wrap(err, "writing jwt svid")
	}

	s.log.InfoContext(
		ctx,
		"Successfully wrote JWT workload identity credential to destination",
		"workload_identity", workloadidentity.WorkloadIdentityLogValue(cred),
		"destination", s.cfg.Destination.String(),
	)
	return nil
}
