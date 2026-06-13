package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

func newDiffCmd() *cobra.Command {
	var (
		all    bool
		data   bool
		sample int
	)
	cmd := &cobra.Command{
		Use:   "diff NAME",
		Short: "Show what changed in a branch relative to its base (schema diff + row-count deltas)",
		Long: `Show what changed in a branch relative to the base it was cloned from:
a unified diff of pg_dump --schema-only output, then per-table row-count
deltas. Row counts are planner estimates (pg_class.reltuples), not exact.
The engine provisions a temporary clone of the branch's base per call, so
expect a few seconds.

With --data, up to --sample (default 20) branch-only rows are shown per
grown table — rows present on the branch but not the base, matched by
primary key. Tables without a primary key are skipped (noted in a footer).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n := 0
			if data {
				n = sample
				if n <= 0 {
					n = 20
				}
			}
			var res *engine.DiffResult
			if c := serverClient(cmd); c != nil {
				r, err := c.DiffBranch(cmd.Context(), args[0], n)
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
				var opts []engine.DiffOption
				if n > 0 {
					opts = append(opts, engine.WithDataSample(n))
				}
				r, err := e.DiffBranch(cmd.Context(), args[0], opts...)
				if err != nil {
					return err
				}
				res = r
			}
			return renderDiff(cmd.OutOrStdout(), res, all, data)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every table, not just those with row-count changes")
	cmd.Flags().BoolVar(&data, "data", false, "include a bounded sample of branch-only rows for grown tables")
	cmd.Flags().IntVar(&sample, "sample", 20, "max sample rows per table (with --data)")
	return cmd
}

// renderDiff prints the schema diff verbatim, then the row-estimate table
// (changed tables only unless all is set), then, when data is set, up to the
// requested number of branch-only sample rows per grown table.
func renderDiff(w io.Writer, res *engine.DiffResult, all, data bool) error {
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
	} else {
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
	}
	if data {
		if err := renderSamples(w, res); err != nil {
			return err
		}
	}
	return nil
}

// renderSamples prints up to the requested branch-only sample rows per grown
// table as compact JSON, and notes the tables that grew but were skipped for
// lacking a primary key.
func renderSamples(w io.Writer, res *engine.DiffResult) error {
	var skipped []string
	printedAny := false
	for _, t := range res.Tables {
		grew := t.BranchRows > t.BaseRows
		if !grew {
			continue
		}
		if len(t.SampleRows) == 0 {
			// grew but no samples: either no PK (skipped) or no new-by-PK rows
			skipped = append(skipped, t.Table)
			continue
		}
		printedAny = true
		fmt.Fprintf(w, "\nnew rows in %s (up to %d):\n", t.Table, len(t.SampleRows))
		for _, row := range t.SampleRows {
			b, err := json.Marshal(compactRow(row))
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  %s\n", b)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "\n(no sampleable rows for %v — tables without a primary key are skipped)\n", skipped)
	}
	if !printedAny && len(skipped) == 0 {
		fmt.Fprintln(w, "\n(no branch-only rows sampled)")
	}
	return nil
}

// compactRow returns row as an ordered key/value structure so the compact-JSON
// rendering is deterministic (map iteration order is not).
func compactRow(row map[string]any) json.Marshaler {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return orderedRow{keys: keys, row: row}
}

type orderedRow struct {
	keys []string
	row  map[string]any
}

func (o orderedRow) MarshalJSON() ([]byte, error) {
	var b []byte
	b = append(b, '{')
	for i, k := range o.keys {
		if i > 0 {
			b = append(b, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b = append(b, kb...)
		b = append(b, ':')
		vb, err := json.Marshal(o.row[k])
		if err != nil {
			return nil, err
		}
		b = append(b, vb...)
	}
	b = append(b, '}')
	return b, nil
}
