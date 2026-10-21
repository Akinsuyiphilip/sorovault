package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/config"
	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

func newAddCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "add <contract_id>...",
		Short: "Fetch, decode and register one or more contracts",
		Long: `Fetch each contract's WASM from the network, decode the interface
embedded in it, and store the result.

Adding a contract that is already registered is not an error: it re-checks
the contract against the network and records a new interface version if the
contract has been upgraded since it was last seen.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				var failed int

				for _, contractID := range args {
					fetchCtx, cancel := a.fetchContext(ctx)
					result, err := a.registry.Register(fetchCtx, contractID)
					cancel()

					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", contractID, err)
						failed++
						continue
					}

					if asJSON {
						if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
							return err
						}
						continue
					}
					printRegisterResult(cmd.OutOrStdout(), result)
				}

				if failed > 0 {
					return fmt.Errorf("%w: %d of %d contract(s) failed", errPrinted, failed, len(args))
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full result as JSON")
	return cmd
}

func newRefreshCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "refresh <contract_id>...",
		Short: "Re-check registered contracts for an upgraded interface",
		Long: `Compare each registered contract against the network and record a new
interface version if its WASM has changed.

Unlike "add", refresh starts from what is already stored, so an unchanged
contract costs a single RPC round trip: the module is only downloaded once
its hash is known to differ.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				var failed int

				for _, contractID := range args {
					fetchCtx, cancel := a.fetchContext(ctx)
					result, err := a.registry.Refresh(fetchCtx, contractID)
					cancel()

					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", contractID, err)
						failed++
						continue
					}

					if asJSON {
						if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
							return err
						}
						continue
					}

					if result.Changed {
						fmt.Fprintf(cmd.OutOrStdout(), "%s upgraded to %s\n",
							result.Contract.ContractID, result.Contract.CurrentWasmHash)
						continue
					}
					if result.Missing {
						fmt.Fprintf(cmd.OutOrStdout(), "%s missing from the network\n",
							result.Contract.ContractID)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s unchanged\n", result.Contract.ContractID)
				}

				if failed > 0 {
					return fmt.Errorf("%w: %d of %d contract(s) failed", errPrinted, failed, len(args))
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full result as JSON")
	return cmd
}

func newListCmd() *cobra.Command {
	var (
		query     string
		network   string
		functions []string
		types     []string
		limit     int
		offset    int
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered contracts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				filter := store.ListFilter{
					Query:     query,
					Network:   network,
					Functions: functions,
					Types:     types,
					Limit:     limit,
					Offset:    offset,
				}

				page, err := a.registry.Store().ListContracts(ctx, filter)
				if err != nil {
					return err
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), page)
				}

				if len(page.Contracts) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "no contracts registered")
					return nil
				}

				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "CONTRACT ID\tNETWORK\tWASM HASH\tLAST REFRESHED\tMATCHES\tSTATUS")
				for _, c := range page.Contracts {
					status := ""
					if c.MissingSince != nil {
						status = "missing"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						c.ContractID, c.Network, truncate(c.CurrentWasmHash, 12),
						c.LastRefreshed.UTC().Format("2006-01-02 15:04"),
						strings.Join(c.Matches, ","), status)
				}
				if err := tw.Flush(); err != nil {
					return err
				}

				fmt.Fprintf(cmd.ErrOrStderr(), "\n%d of %d contract(s)\n", len(page.Contracts), page.Total)
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&query, "query", "q", "", "search contract ID, name, and function, type and event names")
	cmd.Flags().StringVar(&network, "network", "", "restrict to one network")
	cmd.Flags().StringArrayVar(&functions, "function", nil, "only contracts with a function of exactly this name (repeatable)")
	cmd.Flags().StringArrayVar(&types, "type", nil, "only contracts with a user-defined type of exactly this name (repeatable)")
	cmd.Flags().IntVar(&limit, "limit", store.DefaultLimit, "maximum rows to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit results as JSON")
	return cmd
}

func newGetCmd() *cobra.Command {
	var (
		network  string
		wasmHash string
		function string
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "get <contract_id>",
		Short: "Show a registered contract's decoded interface",
		Long: `Print a contract's decoded interface.

With --json the output is the same ABI document the HTTP API serves, so it
can be piped straight into other tooling.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				contractID := args[0]

				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				stored, err := a.registry.Store().GetSpec(ctx, net, contractID, wasmHash)
				if err != nil {
					return err
				}

				if function != "" {
					fn, ok := stored.Interface.Function(function)
					if !ok {
						return fmt.Errorf("no function %q in contract %s", function, contractID)
					}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), fn)
					}
					printFunction(cmd.OutOrStdout(), fn)
					return nil
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), stored.Interface)
				}
				printInterface(cmd.OutOrStdout(), stored)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().StringVar(&wasmHash, "wasm-hash", "", "show a specific version instead of the current one")
	cmd.Flags().StringVar(&function, "function", "", "show only this function")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the ABI as JSON")
	return cmd
}

func newVersionsCmd() *cobra.Command {
	var (
		network string
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "versions <contract_id>",
		Short: "List a contract's stored interface versions",
		Long: `Print every interface SoroVault has stored for a contract, newest first.

The current version is the one the contract runs now; an older one can be
read with "get --wasm-hash".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				versions, err := a.registry.Store().ListVersions(ctx, net, args[0])
				if err != nil {
					return err
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), versionsDocument{
						ContractID: args[0],
						Network:    net,
						Versions:   versions,
					})
				}
				printVersions(cmd.OutOrStdout(), versions)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the versions as JSON")
	return cmd
}

func newCodegenCmd() *cobra.Command {
	var (
		network     string
		wasmHash    string
		lang        string
		output      string
		asPackage   bool
		packageName string
		pkgVersion  string
	)

	cmd := &cobra.Command{
		Use:   "codegen <contract_id>",
		Short: "Generate a typed client for a registered contract",
		Long: `Generate a typed client from a registered contract's stored interface.

The client is generated from what SoroVault already holds, so no network
access is needed. --wasm-hash generates against a superseded version.

Supported formats are TypeScript (ts) and Markdown (md). With --package the
output is a complete npm package: the client under src/, a package.json that
pins @stellar/stellar-sdk and buffer as peer dependencies, a tsconfig.json
and a README, ready to publish or install from a tarball.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang != "ts" && lang != "typescript" && lang != "md" && lang != "markdown" {
				return fmt.Errorf("unsupported --lang %q (supported: ts, md)", lang)
			}
			if asPackage {
				if lang == "md" || lang == "markdown" {
					return fmt.Errorf("--package writes a TypeScript package, so --lang md cannot be packaged")
				}
				if strings.TrimSpace(packageName) == "" {
					return fmt.Errorf("--package needs --name: the npm package name to publish")
				}
			} else if pkgVersion != "" {
				return fmt.Errorf("--package-version only applies to --package")
			}

			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				stored, err := a.registry.Store().GetSpec(ctx, net, args[0], wasmHash)
				if err != nil {
					return err
				}

				src := codegen.Source{
					ContractID: stored.ContractID,
					Network:    stored.Network,
					WasmHash:   stored.WasmHash,
				}

				if asPackage {
					files, err := codegen.Package(stored.Interface, src, codegen.PackageOptions{
						Name:    packageName,
						Version: pkgVersion,
					})
					if err != nil {
						return err
					}

					dir := output
					if dir == "" || dir == "-" {
						dir = packageDirName(packageName)
					}
					for _, file := range files {
						path := filepath.Join(dir, filepath.FromSlash(file.Path))
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							return err
						}
						if err := os.WriteFile(path, file.Data, 0o644); err != nil {
							return err
						}
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d files)\n", dir, len(files))
					return nil
				}

				var source string
				if lang == "md" || lang == "markdown" {
					source, err = codegen.Markdown(stored.Interface, src)
				} else {
					source, err = codegen.TypeScript(stored.Interface, src)
				}
				if err != nil {
					return err
				}

				if output == "" || output == "-" {
					_, err = io.WriteString(cmd.OutOrStdout(), source)
					return err
				}
				if err := os.WriteFile(output, []byte(source), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", output)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().StringVar(&wasmHash, "wasm-hash", "", "generate for a specific version instead of the current one")
	cmd.Flags().StringVar(&lang, "lang", "ts", "output format (ts or md)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file, or this directory with --package")
	cmd.Flags().BoolVar(&asPackage, "package", false, "write a complete npm package instead of one file")
	cmd.Flags().StringVar(&packageName, "name", "", "npm package name for --package (@scope/name)")
	cmd.Flags().StringVar(&pkgVersion, "package-version", "", "version for --package (default: derived from the WASM hash)")
	return cmd
}

// packageDirName is the directory --package writes to when --output is not
// given. A scope is part of the package name, not a directory to create, so
// @acme/zkvote becomes zkvote.
func packageDirName(name string) string {
	if _, base, ok := strings.Cut(name, "/"); ok {
		return base
	}
	return name
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseURL, err := requireDatabaseURL()
			if err != nil {
				return err
			}

			if err := store.Migrate(databaseURL); err != nil {
				return err
			}

			version, dirty, err := store.MigrationVersion(databaseURL)
			if err != nil {
				return err
			}
			if dirty {
				return fmt.Errorf("database is at version %d but marked dirty; a previous migration failed partway and needs manual repair", version)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "database is at migration version %d\n", version)
			return nil
		},
	}
	return cmd
}

// doctorCheck is one thing a doctor run checks.
type doctorCheck struct {
	// Name identifies the check in the report.
	Name string
	// Run performs the check. It returns a line describing what it found,
	// or an error if the check failed.
	Run func(ctx context.Context) (string, error)
}

// doctorResult is the outcome of one check.
type doctorResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Detail describes what the check found.
	Detail string `json:"detail,omitempty"`
	// Error says why the check failed.
	Error string `json:"error,omitempty"`
}

// doctorReport is the outcome of a whole doctor run.
type doctorReport struct {
	OK     bool           `json:"ok"`
	Checks []doctorResult `json:"checks"`
}

// failed counts the checks that did not pass.
func (r doctorReport) failed() int {
	n := 0
	for _, c := range r.Checks {
		if !c.OK {
			n++
		}
	}
	return n
}

// doctorDeps are the things a doctor run talks to. They are injected so the
// command can be exercised without a database or a network.
type doctorDeps struct {
	// Ping checks that the database answers.
	Ping func(ctx context.Context) error
	// Migrations reports the migration version the database has applied,
	// the latest embedded one, and whether the database is marked dirty.
	Migrations func(ctx context.Context) (current, latest uint, dirty bool, err error)
	// Passphrase reads the passphrase the RPC endpoint serves.
	Passphrase func(ctx context.Context) (string, error)
	// ExpectPassphrase is the network the operator configured.
	ExpectPassphrase string
	// Close releases whatever the dependencies hold.
	Close func() error
}

// newDoctorCmd builds the doctor command. Its dependencies are injected so
// the command can be exercised without a database or a network.
func newDoctorCmd(depsFor func() (doctorDeps, error)) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that a deployment is healthy",
		Long: `Check the things that are wrong most often: whether the database
answers, whether its schema is up to date, whether the RPC endpoint is
reachable, and whether it serves the network this instance expects.

Every check is read-only. Nothing is written to the database, no migration
is applied, and nothing is changed on the network.`,
		Args: cobra.NoArgs,
		// The report already says which checks failed, so the error only
		// carries the count and cobra need not print it as well.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, err := depsFor()
			if err != nil {
				return err
			}
			if deps.Close != nil {
				defer func() { _ = deps.Close() }()
			}

			report := runDoctor(cmd.Context(), doctorChecks(deps))

			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), report)
			}

			if !report.OK {
				return fmt.Errorf("%d of %d checks failed", report.failed(), len(report.Checks))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// doctorDepsFromEnv builds the doctor's dependencies from the environment.
func doctorDepsFromEnv() (doctorDeps, error) {
	cfg, err := config.Load()
	if err != nil {
		return doctorDeps{}, err
	}
	return newDoctorDeps(cfg)
}

// newDoctorDeps builds the doctor's dependencies from the configuration:
// the database at DATABASE_URL and the RPC endpoint at RPC_URL.
//
// The passphrase is read once and remembered, so the reachability and the
// passphrase check cost one round trip between them.
func newDoctorDeps(cfg *config.Config) (doctorDeps, error) {
	fetcher := stellar.NewRPCFetcher(cfg.RPCURL, nil)

	var (
		once       sync.Once
		passphrase string
		passErr    error
	)
	readPassphrase := func(ctx context.Context) (string, error) {
		once.Do(func() { passphrase, passErr = fetcher.Network(ctx) })
		return passphrase, passErr
	}

	return doctorDeps{
		Ping: func(ctx context.Context) error {
			return store.Ping(ctx, cfg.DatabaseURL)
		},
		Migrations: func(ctx context.Context) (uint, uint, bool, error) {
			return store.MigrationStatus(ctx, cfg.DatabaseURL)
		},
		Passphrase:       readPassphrase,
		ExpectPassphrase: cfg.NetworkPassphrase,
		Close:            fetcher.Close,
	}, nil
}

// doctorChecks builds the checks a doctor run performs, in the order they
// are reported.
func doctorChecks(deps doctorDeps) []doctorCheck {
	return []doctorCheck{
		{
			Name: "database",
			Run: func(ctx context.Context) (string, error) {
				if err := deps.Ping(ctx); err != nil {
					return "", err
				}
				return "the database answers", nil
			},
		},
		{
			Name: "migrations",
			Run: func(ctx context.Context) (string, error) {
				current, latest, dirty, err := deps.Migrations(ctx)
				if err != nil {
					return "", err
				}
				if dirty {
					return "", fmt.Errorf("the database is at version %d but marked dirty; a previous migration failed partway and needs manual repair", current)
				}
				if current < latest {
					return "", fmt.Errorf("the database is at version %d, but version %d is available; run `sorovault migrate`", current, latest)
				}
				return fmt.Sprintf("the database is at version %d, the latest is %d", current, latest), nil
			},
		},
		{
			Name: "rpc",
			Run: func(ctx context.Context) (string, error) {
				passphrase, err := deps.Passphrase(ctx)
				if err != nil {
					return "", err
				}
				return "the endpoint is reachable, serving " + stellar.NetworkName(passphrase), nil
			},
		},
		{
			Name: "passphrase",
			Run: func(ctx context.Context) (string, error) {
				passphrase, err := deps.Passphrase(ctx)
				if err != nil {
					return "", err
				}
				if err := registry.CheckPassphrase(passphrase, deps.ExpectPassphrase); err != nil {
					return "", err
				}
				return "the endpoint serves the configured network", nil
			},
		},
	}
}

// runDoctor runs every check and collects the results. A check that fails
// does not stop the ones after it: the point of a doctor run is to say
// everything that is wrong, not just the first thing.
func runDoctor(ctx context.Context, checks []doctorCheck) doctorReport {
	report := doctorReport{OK: true, Checks: make([]doctorResult, 0, len(checks))}

	for _, check := range checks {
		detail, err := check.Run(ctx)
		result := doctorResult{Name: check.Name, OK: err == nil, Detail: detail}
		if err != nil {
			result.Error = err.Error()
			report.OK = false
		}
		report.Checks = append(report.Checks, result)
	}

	return report
}

// printDoctor writes the report as one line per check.
func printDoctor(w io.Writer, report doctorReport) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range report.Checks {
		status, detail := "PASS", c.Detail
		if !c.OK {
			status, detail = "FAIL", c.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", status, c.Name, detail)
	}
	_ = tw.Flush()
}

// printRegisterResult writes the human-readable summary of an add.
func printRegisterResult(w io.Writer, r *registry.Result) {
	verb := "already current"
	switch {
	case r.Missing:
		verb = "missing from the network"
	case r.Created:
		verb = "registered"
	case r.Changed:
		verb = "upgraded"
	}

	fmt.Fprintf(w, "%s %s on %s\n", r.Contract.ContractID, verb, r.Contract.Network)
	fmt.Fprintf(w, "  wasm hash  %s\n", r.Contract.CurrentWasmHash)

	if r.Interface != nil {
		fmt.Fprintf(w, "  functions  %d\n", len(r.Interface.Functions))
		if n := len(r.Interface.Types.Structs) + len(r.Interface.Types.Unions) +
			len(r.Interface.Types.Enums) + len(r.Interface.Types.ErrorEnums); n > 0 {
			fmt.Fprintf(w, "  types      %d\n", n)
		}
		if n := len(r.Interface.Events); n > 0 {
			fmt.Fprintf(w, "  events     %d\n", n)
		}
	}
}

// printInterface writes a contract's interface as readable signatures.
func printInterface(w io.Writer, s *store.Spec) {
	fmt.Fprintf(w, "%s  (%s)\n", s.ContractID, s.Network)
	fmt.Fprintf(w, "wasm %s\n\n", s.WasmHash)

	if len(s.Interface.Functions) == 0 {
		fmt.Fprintln(w, "no functions")
	}
	for _, fn := range s.Interface.Functions {
		fmt.Fprintf(w, "  %s\n", fn.Signature())
	}

	t := s.Interface.Types
	if n := len(t.Structs) + len(t.Unions) + len(t.Enums) + len(t.ErrorEnums); n > 0 {
		fmt.Fprintln(w, "\ntypes:")
		for _, x := range t.Structs {
			fmt.Fprintf(w, "  struct %s (%d fields)\n", x.Name, len(x.Fields))
		}
		for _, x := range t.Unions {
			fmt.Fprintf(w, "  enum   %s (%d cases)\n", x.Name, len(x.Cases))
		}
		for _, x := range t.Enums {
			fmt.Fprintf(w, "  enum   %s (%d cases, u32)\n", x.Name, len(x.Cases))
		}
		for _, x := range t.ErrorEnums {
			fmt.Fprintf(w, "  error  %s (%d cases)\n", x.Name, len(x.Cases))
		}
	}

	if len(s.Interface.Events) > 0 {
		fmt.Fprintln(w, "\nevents:")
		for _, e := range s.Interface.Events {
			fmt.Fprintf(w, "  %s (%d params)\n", e.Name, len(e.Params))
		}
	}
}

// versionsDocument is the JSON shape of the versions command. It matches
// what GET /api/contracts/{id}/versions serves, so the two are
// interchangeable in a pipeline.
type versionsDocument struct {
	ContractID string              `json:"contract_id"`
	Network    string              `json:"network"`
	Versions   []store.SpecVersion `json:"versions"`
}

// printVersions writes one row per stored interface, newest first.
func printVersions(w io.Writer, versions []store.SpecVersion) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WASM HASH\tDECODED\tCURRENT")
	for _, v := range versions {
		current := ""
		if v.Current {
			current = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			v.WasmHash, v.DecodedAt.UTC().Format("2006-01-02 15:04"), current)
	}
	_ = tw.Flush()
}

// printFunction writes one function's signature in detail.
func printFunction(w io.Writer, fn model.Function) {
	fmt.Fprintf(w, "%s\n", fn.Signature())

	if fn.Doc != "" {
		for _, line := range strings.Split(strings.TrimRight(fn.Doc, "\n"), "\n") {
			fmt.Fprintf(w, "  // %s\n", line)
		}
	}

	if len(fn.Inputs) > 0 {
		fmt.Fprintln(w, "\ninputs:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, in := range fn.Inputs {
			fmt.Fprintf(tw, "  %s\t%s\n", in.Name, in.Type.Display)
		}
		_ = tw.Flush()
	}

	fmt.Fprint(w, "\nreturns: ")
	if len(fn.Outputs) == 0 {
		fmt.Fprintln(w, "()")
		return
	}
	fmt.Fprintln(w, fn.Outputs[0].Display)
}

// writeJSON emits an indented JSON document followed by a newline.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
