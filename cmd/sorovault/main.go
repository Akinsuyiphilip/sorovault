// Command sorovault registers Soroban contracts and serves their decoded
// interfaces over HTTP.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error; exit non-zero without
		// repeating it.
		if !errors.Is(err, errPrinted) {
			fmt.Fprintln(os.Stderr, "sorovault:", err)
		}
		os.Exit(1)
	}
}

// errPrinted marks an error whose message has already reached the user.
var errPrinted = errors.New("error already reported")

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sorovault",
		Short: "A contract metadata and interface registry for Soroban",
		Long: `SoroVault fetches a deployed Soroban contract's WASM, decodes the
interface embedded in it, and stores the result in a searchable registry
that both people and tooling can read.

Configuration is read from the environment:

  RPC_URL             Stellar RPC endpoint      (default testnet)
  NETWORK_PASSPHRASE  expected network          (default testnet)
  DATABASE_URL        Postgres connection URL   (required; or DATABASE_URL_FILE)
  HTTP_ADDR           listen address for serve  (default :8080)
  LOG_LEVEL           debug|info|warn|error     (default info)`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		newAddCmd(),
		newListCmd(),
		newGetCmd(),
		newCodegenCmd(),
		newRefreshCmd(),
		newServeCmd(),
		newMigrateCmd(),
	)

	return root
}
