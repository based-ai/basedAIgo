// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package run

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ava-labs/avalanchego/vms/example/proxyvm"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
)

func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "proxyvm",
		Short: "Runs a ProxyVM plugin",
		RunE:  runFunc,
	}
}

func runFunc(*cobra.Command, []string) error {
	return rpcchainvm.Serve(context.Background(), &proxyvm.VM{})
}
