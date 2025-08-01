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

package main

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/lib/tbot/cli"
	"github.com/gravitational/teleport/lib/tbot/config"
	"github.com/gravitational/teleport/lib/tbot/tshwrap"
)

func onDBCommand(globalCfg *cli.GlobalArgs, dbCmd *cli.DBCommand) error {
	botConfig, err := cli.LoadConfigWithMutators(globalCfg)
	if err != nil {
		return trace.Wrap(err)
	}

	wrapper, err := tshwrap.New()
	if err != nil {
		return trace.Wrap(err)
	}

	destination, err := tshwrap.GetDestinationDirectory(dbCmd.DestinationDir, botConfig)
	if err != nil {
		return trace.Wrap(err)
	}

	env, err := tshwrap.GetEnvForTSH(destination.Path)
	if err != nil {
		return trace.Wrap(err)
	}

	identityPath := filepath.Join(destination.Path, config.IdentityFilePath)
	identity, err := tshwrap.LoadIdentity(identityPath)
	if err != nil {
		return trace.Wrap(err)
	}

	args := []string{"-i", identityPath, "db", "--proxy=" + dbCmd.ProxyServer}
	if dbCmd.Cluster != "" {
		// If we caught --cluster in our args, pass it through.
		args = append(args, "--cluster="+dbCmd.Cluster)
	} else if !slices.ContainsFunc(*dbCmd.RemainingArgs, func(s string) bool { return strings.HasPrefix(s, "--cluster") }) {
		// If no `--cluster` was provided after a `--`, pass along the cluster
		// name in the identity.
		args = append(args, "--cluster="+identity.RouteToCluster)
	}
	args = append(args, *dbCmd.RemainingArgs...)

	// Pass through the debug flag, and prepend to satisfy argument ordering
	// needs (`-d` must precede `db`).
	if botConfig.Debug {
		args = append([]string{"-d"}, args...)
	}

	return trace.Wrap(wrapper.Exec(env, args...), "executing `tsh db`")
}
