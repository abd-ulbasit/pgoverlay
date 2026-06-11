package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

func newDiffCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "diff NAME",
		Short: "Show what changed in a branch relative to its base (schema diff + row-count deltas)",
		Long: `Show what changed in a branch relative to the base it was cloned from:
a unified diff of pg_dump --schema-only output, then per-table row-count
deltas. Row counts are planner estimates (pg_class.reltuples), not exact.
The engine provisions a temporary clone of the branch's base per call, so
expect a few seconds.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res *engine.DiffResult
			if c := serverClient(cmd); c != nil {
				r, err := c.DiffBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				res = r
			} else {
				e, reg, err := open()
				if err != nil {
					return err
				}
				defer reg.Close()
				r, err := e.DiffBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				res = r
			}
			return renderDiff(cmd.OutOrStdout(), res, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every table, not just those with row-count changes")
	return cmd
}

// renderDiff prints the schema diff verbatim, then the row-estimate table
// (changed tables only unless all is set).
func renderDiff(w io.Writer, res *engine.DiffResult, all bool) error {
	if res.SchemaDiff == "" {
		fmt.Fprintln(w, "schema: no differences")
	} else {
		fmt.Fprint(w, res.SchemaDiff)
	}
	rows := res.Tables
	if !all {
		rows = nil
		for _, t := range res.Tables {
			if t.Delta != 0 {
				rows = append(rows, t)
			}
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "tables: no row-count changes")
		return nil
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tBASE\tBRANCH\tDELTA")
	for _, t := range rows {
		delta := fmt.Sprintf("%+d", t.Delta)
		if t.Delta == 0 {
			delta = "0"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", t.Table, t.BaseRows, t.BranchRows, delta)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "(row counts are planner estimates)")
	return nil
}
