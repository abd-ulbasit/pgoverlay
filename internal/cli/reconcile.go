package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// Process exit statuses (see ExitCode). Every command exits ExitError on
// failure, except `pgb doctor`, which separates drift from not knowing.
const (
	ExitError = 1
	// ExitDrift: `pgb doctor` computed the plan and found drift.
	ExitDrift = 1
	// ExitDoctorFailed: `pgb doctor` could not compute the plan (branchd
	// unreachable, bad token, bad flags), so it cannot say whether there is
	// drift.
	ExitDoctorFailed = 2
)

// exitOnErrorAnnotation on a command overrides ExitError for its failures
// other than drift.
const exitOnErrorAnnotation = "pgoverlay.io/exit-on-error"

// ExitCode maps the command that ran and the error it returned (as from
// cobra's ExecuteC) to the process exit status. Drift is ExitDrift; any other
// failure is ExitError unless the command sets exitOnErrorAnnotation.
func ExitCode(cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}
	var d errDrift
	if errors.As(err, &d) {
		return ExitDrift
	}
	if cmd != nil {
		if n, convErr := strconv.Atoi(cmd.Annotations[exitOnErrorAnnotation]); convErr == nil {
			return n
		}
	}
	return ExitError
}

// newDoctorCmd reports reconcile drift read-only: stuck rows, orphaned
// containers, dangling layers/volumes. It mutates nothing and exits 1 when
// drift is found (CI-friendly: `pgb doctor && deploy`), and 2 when it could
// not compute the plan, so a gate can treat the two differently.
func newDoctorCmd() *cobra.Command {
	var stuck time.Duration
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report reconcile drift (orphans, stuck rows, dangling layers/volumes) read-only",
		Long: "doctor computes the reconcile plan and prints it without changing anything, so it can gate CI. " +
			"Run `pgb gc` to apply the plan.\n\n" +
			"Exit status: 0 no drift; 1 drift found; 2 the plan could not be computed " +
			"(branchd unreachable, authentication failed, invalid flags).",
		Annotations: map[string]string{exitOnErrorAnnotation: strconv.Itoa(ExitDoctorFailed)},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := planReconcile(cmd, stuck)
			if err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), *plan, "drift")
			if plan.Drift() {
				// signal drift with a non-zero exit, but don't print usage.
				return errDrift{n: len(plan.Actions)}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&stuck, "stuck-timeout", api.DefaultStuckTimeout,
		"local mode: age past which a creating/resetting row is considered stuck")
	return cmd
}

// newGCCmd applies the reconcile plan: reaps expired branches, fails stuck
// rows, removes orphan containers, GCs dangling layers/volumes.
func newGCCmd() *cobra.Command {
	var stuck time.Duration
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Apply the reconcile plan (reap, fail stuck rows, remove orphans, GC layers/volumes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			taken, err := applyReconcile(cmd, stuck)
			if err != nil {
				return err
			}
			if !taken.Drift() {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to reconcile; already converged")
				return nil
			}
			printPlan(cmd.OutOrStdout(), *taken, "applied")
			return nil
		},
	}
	cmd.Flags().DurationVar(&stuck, "stuck-timeout", api.DefaultStuckTimeout,
		"local mode: age past which a creating/resetting row is considered stuck")
	return cmd
}

// errDrift makes `pgb doctor` exit non-zero on drift without a usage dump.
type errDrift struct{ n int }

func (e errDrift) Error() string {
	return fmt.Sprintf("drift: %d action(s) pending; run `pgb gc`", e.n)
}

func planReconcile(cmd *cobra.Command, stuck time.Duration) (*engine.ReconcilePlan, error) {
	if c := serverClient(cmd); c != nil {
		return c.ReconcilePlan(cmd.Context())
	}
	e, reg, err := open()
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	p, err := e.PlanReconcile(cmd.Context(), time.Now(), stuck)
	return &p, err
}

func applyReconcile(cmd *cobra.Command, stuck time.Duration) (*engine.ReconcilePlan, error) {
	if c := serverClient(cmd); c != nil {
		return c.ReconcileApply(cmd.Context())
	}
	e, reg, err := open()
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	p, err := e.ApplyReconcile(cmd.Context(), time.Now(), stuck)
	return &p, err
}

// printPlan tabulates a plan. verb is the column header context ("drift" vs
// "applied").
func printPlan(out io.Writer, plan engine.ReconcilePlan, verb string) {
	if !plan.Drift() {
		fmt.Fprintln(out, "no "+verb+" found; registry and reality agree")
		return
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tTARGET\tREASON")
	for _, a := range plan.Actions {
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.Kind, a.Target, a.Reason)
	}
	w.Flush()
	fmt.Fprintf(out, "%d %s action(s)\n", len(plan.Actions), verb)
}
