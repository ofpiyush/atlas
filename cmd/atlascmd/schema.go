// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package atlascmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

var (
	// schemaCmd represents the subcommand 'atlas schema'.
	schemaCmd = &cobra.Command{
		Use:   "schema",
		Short: "Work with atlas schemas.",
		Long:  "The `atlas schema` command groups subcommands for working with Atlas schemas.",
	}

	// ApplyFlags are the flags used in SchemaApply command.
	ApplyFlags struct {
		URL         string
		DevURL      string
		File        string
		Web         bool
		Addr        string
		DryRun      bool
		Schema      []string
		AutoApprove bool
		Verbose     bool
		Vars        map[string]string
	}
	// SchemaApply represents the 'atlas schema apply' subcommand command.
	SchemaApply = &cobra.Command{
		Use:   "apply",
		Short: "Apply an atlas schema to a target database.",
		// Use 80-columns as max width.
		Long: `'atlas schema apply' plans and executes a database migration to bring a given
database to the state described in the Atlas schema file. Before running the
migration, Atlas will print the migration plan and prompt the user for approval.

If run with the "--dry-run" flag, atlas will exit after printing out the planned
migration.`,
		RunE: CmdApplyRun,
		Example: `  atlas schema apply -u "mysql://user:pass@localhost/dbname" -f atlas.hcl
  atlas schema apply -u "mysql://localhost" -f atlas.hcl --schema prod --schema staging
  atlas schema apply -u "mysql://user:pass@localhost:3306/dbname" -f atlas.hcl --dry-run
  atlas schema apply -u "mariadb://user:pass@localhost:3306/dbname" -f atlas.hcl
  atlas schema apply --url "postgres://user:pass@host:port/dbname?sslmode=disable" -f atlas.hcl
  atlas schema apply -u "sqlite://file:ex1.db?_fk=1" -f atlas.hcl`,
	}

	// InspectFlags are the flags used in SchemaInspect command.
	InspectFlags struct {
		URL    string
		Web    bool
		Addr   string
		Schema []string
	}
	// SchemaInspect represents the 'atlas schema inspect' subcommand.
	SchemaInspect = &cobra.Command{
		Use:   "inspect",
		Short: "Inspect a database's and print its schema in Atlas DDL syntax.",
		Long: `'atlas schema inspect' connects to the given database and inspects its schema.
It then prints to the screen the schema of that database in Atlas DDL syntax. This output can be
saved to a file, commonly by redirecting the output to a file named with a ".hcl" suffix:

  atlas schema inspect -u "mysql://user:pass@localhost:3306/dbname" > atlas.hcl

This file can then be edited and used with the` + " `atlas schema apply` " + `command to plan
and execute schema migrations against the given database. In cases where users wish to inspect
all multiple schemas in a given database (for instance a MySQL server may contain multiple named
databases), omit the relevant part from the url, e.g. "mysql://user:pass@localhost:3306/".
To select specific schemas from the databases, users may use the "--schema" (or "-s" shorthand)
flag.
	`,
		Run: CmdInspectRun,
		Example: `  atlas schema inspect -u "mysql://user:pass@localhost:3306/dbname"
  atlas schema inspect -u "mariadb://user:pass@localhost:3306/" --schema=schemaA,schemaB -s schemaC
  atlas schema inspect --url "postgres://user:pass@host:port/dbname?sslmode=disable"
  atlas schema inspect -u "sqlite://file:ex1.db?_fk=1"`,
	}

	// SchemaFmt represents the 'atlas schema fmt' subcommand.
	SchemaFmt = &cobra.Command{
		Use:   "fmt [path ...]",
		Short: "Formats Atlas HCL files",
		Long: `'atlas schema fmt' formats all ".hcl" files under the given path using
canonical HCL layout style as defined by the github.com/hashicorp/hcl/v2/hclwrite package.
Unless stated otherwise, the fmt command will use the current directory.

After running, the command will print the names of the files it has formatted. If all
files in the directory are formatted, no input will be printed out.
`,
		Run: CmdFmtRun,
	}
)

const (
	answerApply = "Apply"
	answerAbort = "Abort"
)

// selectEnv selects the environment config from the current directory project file.
func selectEnv(args []string) (*Env, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return LoadEnv(projectFileName, args[0])
}

func init() {
	// Schema apply flags.
	schemaCmd.AddCommand(SchemaApply)
	SchemaApply.Flags().SortFlags = false
	SchemaApply.Flags().StringVarP(&ApplyFlags.File, "file", "f", "", "[/path/to/file] file containing the HCL schema.")
	SchemaApply.Flags().StringVarP(&ApplyFlags.URL, "url", "u", "", "URL to the database using the format:\n[driver://username:password@address/dbname?param=value]")
	SchemaApply.Flags().StringSliceVarP(&ApplyFlags.Schema, "schema", "s", nil, "Set schema names.")
	SchemaApply.Flags().StringVarP(&ApplyFlags.DevURL, "dev-url", "", "", "URL for the dev database. Used to validate schemas and calculate diffs\nbefore running migration.")
	SchemaApply.Flags().BoolVarP(&ApplyFlags.DryRun, "dry-run", "", false, "Dry-run. Print SQL plan without prompting for execution.")
	SchemaApply.Flags().BoolVarP(&ApplyFlags.AutoApprove, "auto-approve", "", false, "Auto approve. Apply the schema changes without prompting for approval.")
	SchemaApply.Flags().BoolVarP(&ApplyFlags.Web, "web", "w", false, "Open in a local Atlas UI.")
	SchemaApply.Flags().StringVarP(&ApplyFlags.Addr, "addr", "", ":5800", "used with -w, local address to bind the server to.")
	SchemaApply.Flags().BoolVarP(&ApplyFlags.Verbose, migrateDiffFlagVerbose, "", false, "enable verbose logging")
	SchemaApply.Flags().StringToStringVarP(&ApplyFlags.Vars, "var", "", nil, "input variables")
	cobra.CheckErr(SchemaApply.MarkFlagRequired("url"))
	fixURLFlag(SchemaApply, &ApplyFlags.URL)

	// Schema inspect flags.
	schemaCmd.AddCommand(SchemaInspect)
	SchemaInspect.Flags().StringVarP(&InspectFlags.URL, "url", "u", "", "[driver://username:password@protocol(address)/dbname?param=value] Select data source using the url format")
	SchemaInspect.Flags().BoolVarP(&InspectFlags.Web, "web", "w", false, "Open in a local Atlas UI")
	SchemaInspect.Flags().StringVarP(&InspectFlags.Addr, "addr", "", ":5800", "Used with -w, local address to bind the server to")
	SchemaInspect.Flags().StringSliceVarP(&InspectFlags.Schema, "schema", "s", nil, "Set schema name")
	cobra.CheckErr(SchemaInspect.MarkFlagRequired("url"))
	fixURLFlag(SchemaInspect, &InspectFlags.URL)

	// Schema fmt.
	schemaCmd.AddCommand(SchemaFmt)
}

// CmdInspectRun is the command used when running CLI.
func CmdInspectRun(cmd *cobra.Command, args []string) {
	if InspectFlags.Web {
		schemaCmd.PrintErrln("The Alas UI is not available in this release.")
		return
	}
	client, err := sqlclient.Open(cmd.Context(), InspectFlags.URL)
	cobra.CheckErr(err)
	defer client.Close()
	schemas := InspectFlags.Schema
	activeEnv, err := selectEnv(args)
	cobra.CheckErr(err)
	if activeEnv != nil && len(activeEnv.Schemas) > 0 {
		schemas = activeEnv.Schemas
	}
	if client.URL.Schema != "" {
		schemas = append(schemas, client.URL.Schema)
	}
	s, err := client.InspectRealm(cmd.Context(), &schema.InspectRealmOption{
		Schemas: schemas,
	})
	cobra.CheckErr(err)
	ddl, err := client.MarshalSpec(s)
	cobra.CheckErr(err)
	schemaCmd.Print(string(ddl))
}

// CmdApplyRun is the command used when running CLI.
func CmdApplyRun(cmd *cobra.Command, args []string) error {
	if ApplyFlags.Web {
		cmd.Println("The Atlas UI is not available in this release.")
		return errors.New("unavailable")
	}
	c, err := sqlclient.Open(cmd.Context(), ApplyFlags.URL)
	if err != nil {
		return err
	}
	defer c.Close()
	devURL := ApplyFlags.DevURL
	activeEnv, err := selectEnv(args)
	if err != nil {
		return err
	}
	if activeEnv != nil && activeEnv.DevURL != "" {
		devURL = activeEnv.DevURL
	}
	var file string
	switch {
	case activeEnv != nil && activeEnv.Source != "":
		file = activeEnv.Source
	case ApplyFlags.File != "":
		file = ApplyFlags.File
	default:
		return fmt.Errorf("source file must be set via -f or project file")
	}
	if activeEnv != nil && activeEnv.Source != "" {
		file = activeEnv.Source
	}
	return applyRun(cmd.Context(), c, devURL, file, ApplyFlags.DryRun, ApplyFlags.AutoApprove, ApplyFlags.Vars)
}

// CmdFmtRun formats all HCL files in a given directory using canonical HCL formatting
// rules.
func CmdFmtRun(cmd *cobra.Command, args []string) {
	if len(args) == 0 {
		args = append(args, "./")
	}
	for _, path := range args {
		handlePath(cmd, path)
	}
}

func applyRun(ctx context.Context, client *sqlclient.Client, devURL string, file string, dryRun, autoApprove bool, input map[string]string) error {
	schemas := ApplyFlags.Schema
	if client.URL.Schema != "" {
		schemas = append(schemas, client.URL.Schema)
	}
	realm, err := client.InspectRealm(ctx, &schema.InspectRealmOption{
		Schemas: schemas,
	})
	if err != nil {
		return err
	}
	f, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	desired := &schema.Realm{}
	if err := client.Eval(f, desired, input); err != nil {
		return err
	}
	if len(schemas) > 0 {
		// Validate all schemas in file were selected by user.
		sm := make(map[string]bool, len(schemas))
		for _, s := range schemas {
			sm[s] = true
		}
		for _, s := range desired.Schemas {
			if !sm[s.Name] {
				return fmt.Errorf("schema %q from file %q was not selected %q, all schemas defined in file must be selected", s.Name, file, schemas)
			}
		}
	}
	if _, ok := client.Driver.(schema.Normalizer); ok && devURL != "" {
		dev, err := sqlclient.Open(ctx, ApplyFlags.DevURL)
		if err != nil {
			return err
		}
		defer dev.Close()
		desired, err = dev.Driver.(schema.Normalizer).NormalizeRealm(ctx, desired)
		if err != nil {
			return err
		}
	}
	changes, err := client.RealmDiff(realm, desired)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		schemaCmd.Println("Schema is synced, no changes to be made")
		return nil
	}
	p, err := client.PlanChanges(ctx, "plan", changes)
	if err != nil {
		return err
	}
	schemaCmd.Println("-- Planned Changes:")
	for _, c := range p.Changes {
		if c.Comment != "" {
			schemaCmd.Println("--", strings.ToUpper(c.Comment[:1])+c.Comment[1:])
		}
		schemaCmd.Println(c.Cmd)
	}
	if dryRun {
		return nil
	}
	if autoApprove || promptUser() {
		if err := client.ApplyChanges(ctx, changes); err != nil {
			return err
		}
	}
	return nil
}

func promptUser() bool {
	prompt := promptui.Select{
		Label: "Are you sure?",
		Items: []string{answerApply, answerAbort},
	}
	_, result, err := prompt.Run()
	cobra.CheckErr(err)
	return result == answerApply
}

// fixURLFlag fixes the url flag by pulling its value either from the flag itself,
// the (deprecated) dsn flag, or from the active environment.
func fixURLFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVarP(p, "dsn", "d", "", "")
	cobra.CheckErr(cmd.Flags().MarkHidden("dsn"))
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		activeEnv, err := selectEnv(args)
		if err != nil {
			return err
		}
		dsnF, urlF := cmd.Flag("dsn"), cmd.Flag("url")
		switch {
		case activeEnv != nil && activeEnv.URL != "":
			urlF.Changed = true
			if err := urlF.Value.Set(activeEnv.URL); err != nil {
				return err
			}
		case !dsnF.Changed && !urlF.Changed:
			return errors.New(`required flag "url" was not set`)
		case dsnF.Changed && urlF.Changed:
			return errors.New(`both flags "url" and "dsn" were set`)
		case dsnF.Changed && !urlF.Changed:
			urlF.Changed = true
			urlF.Value = dsnF.Value
		}
		return nil
	}
}

func handlePath(cmd *cobra.Command, path string) {
	tasks, err := tasks(path)
	cobra.CheckErr(err)
	for _, task := range tasks {
		changed, err := fmtFile(task)
		cobra.CheckErr(err)
		if changed {
			cmd.Println(task.path)
		}
	}
}

func tasks(path string) ([]fmttask, error) {
	var tasks []fmttask
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		if strings.HasSuffix(path, ".hcl") {
			tasks = append(tasks, fmttask{
				path: path,
				info: stat,
			})
		}
		return tasks, nil
	}
	all, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, f := range all {
		if f.IsDir() {
			continue
		}
		if strings.HasSuffix(f.Name(), ".hcl") {
			tasks = append(tasks, fmttask{
				path: filepath.Join(path, f.Name()),
				info: f,
			})
		}
	}
	return tasks, nil
}

type fmttask struct {
	path string
	info fs.FileInfo
}

// fmtFile tries to format a file and reports if formatting occurred.
func fmtFile(task fmttask) (bool, error) {
	orig, err := os.ReadFile(task.path)
	if err != nil {
		return false, err
	}
	formatted := hclwrite.Format(orig)
	if !bytes.Equal(formatted, orig) {
		return true, os.WriteFile(task.path, formatted, task.info.Mode())
	}
	return false, nil
}
