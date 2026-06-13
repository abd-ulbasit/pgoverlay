package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/diffutil"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// TableDelta is one table's row-estimate comparison between a branch and its
// base. Counts come from pg_class.reltuples — planner estimates, not exact
// counts (fresh never-analyzed tables report 0).
type TableDelta struct {
	Table      string `json:"table"`
	BaseRows   int64  `json:"base_rows"`
	BranchRows int64  `json:"branch_rows"`
	Delta      int64  `json:"delta"`
}

// DiffResult is what changed in a branch relative to its base: a unified
// schema diff (pg_dump --schema-only of base vs branch; empty = identical)
// and per-table row-estimate deltas.
type DiffResult struct {
	SchemaDiff string       `json:"schema_diff"`
	Tables     []TableDelta `json:"tables"`
}

// rowEstimateSQL lists every user table with its planner row estimate, one
// "relname|reltuples" line per table.
const rowEstimateSQL = `SELECT relname || '|' || reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema') ORDER BY relname`

// DiffBranch reports what changed in a ready branch relative to its base. It
// provisions an internal throwaway branch ("diff-<6 hex>") from the target's
// OWN base — the recorded source volume/generation and frozen-layer chain,
// not the source's current generation — then runs pg_dump --schema-only and
// a row-estimate query inside both instances over the local socket (no
// credentials involved, so rotated branch passwords don't matter) and diffs
// host-side. The throwaway is a normal registry row (TTL'd, so the reaper
// cleans strays if branchd dies mid-diff) and is destroyed before returning,
// success or not. Expect a few seconds of wall time: a full branch provision
// plus two dumps.
func (e *Engine) DiffBranch(ctx context.Context, name string) (*DiffResult, error) {
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return nil, err
	}
	if b.State != registry.BranchReady {
		return nil, fmt.Errorf("branch %q is %s, not ready", name, b.State)
	}
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return nil, err
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("diff %q: %w", name, err)
	}
	twName := "diff-" + hex.EncodeToString(suffix)
	// The throwaway copies the target's base coordinates: SourceVolume pins
	// the source generation (zfs/csi children: the parent's dataset/PVC) and
	// BaseLayerID the frozen overlay chain — exactly what reset re-provisions
	// onto. ParentBranchName stays empty: lineage is the diff's business only.
	tw := &registry.Branch{
		Name:         twName,
		SourceID:     b.SourceID,
		RWVolume:     e.planner.BranchLayerName(twName),
		SourceVolume: b.SourceVolume,
		BaseLayerID:  b.BaseLayerID,
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := e.reg.CreateBranch(tw); err != nil {
		return nil, fmt.Errorf("diff %q: %w", name, err)
	}
	// Best-effort teardown on every path; a failure leaves a TTL'd row the
	// reaper retries within the hour.
	defer e.DestroyBranch(context.WithoutCancel(ctx), tw.Name)
	if err := e.provision(ctx, tw, src); err != nil {
		e.reg.TransitionBranch(tw.ID, registry.BranchFailed, err.Error())
		return nil, fmt.Errorf("diff %q: provision base clone: %w", name, err)
	}
	twRow, err := e.reg.GetBranchByName(tw.Name)
	if err != nil {
		return nil, err
	}

	baseDump, err := e.drv.ExecOutput(ctx, twRow.ContainerID, pgDumpSchemaCmd(src))
	if err != nil {
		return nil, fmt.Errorf("diff %q: dump base schema: %w", name, err)
	}
	branchDump, err := e.drv.ExecOutput(ctx, b.ContainerID, pgDumpSchemaCmd(src))
	if err != nil {
		return nil, fmt.Errorf("diff %q: dump branch schema: %w", name, err)
	}
	baseRows, err := e.rowEstimates(ctx, twRow.ContainerID, src)
	if err != nil {
		return nil, fmt.Errorf("diff %q: base row estimates: %w", name, err)
	}
	branchRows, err := e.rowEstimates(ctx, b.ContainerID, src)
	if err != nil {
		return nil, fmt.Errorf("diff %q: branch row estimates: %w", name, err)
	}

	return &DiffResult{
		SchemaDiff: diffutil.Unified(stripDumpNoise(baseDump), stripDumpNoise(branchDump)),
		Tables:     tableDeltas(baseRows, branchRows),
	}, nil
}

// stripDumpNoise removes pg_dump lines that differ between two dumps of the
// same schema for reasons unrelated to structure. pg_dump 16+ brackets its
// output with `\restrict <token>` / `\unrestrict <token>` session-lock
// directives whose token is randomised per run, so they would otherwise show
// as a spurious diff hunk every time.
func stripDumpNoise(dump string) string {
	lines := strings.Split(dump, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(ln, `\restrict `) || strings.HasPrefix(ln, `\unrestrict `) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// pgDumpSchemaCmd builds the in-container schema-only dump over the local
// socket: owners and ACLs are stripped so the diff shows structure, not
// grants noise.
func pgDumpSchemaCmd(src *registry.Source) []string {
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	return []string{"pg_dump", "-U", user, "-h", "/var/run/postgresql", "--schema-only", "--no-owner", "--no-acl", db}
}

// rowEstimates runs the row-estimate query inside the given instance and
// parses its relname|reltuples lines. Negative reltuples (never analyzed)
// clamp to 0.
func (e *Engine) rowEstimates(ctx context.Context, cid string, src *registry.Source) (map[string]int64, error) {
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	cmd := []string{"psql", "-tA", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-h", "/var/run/postgresql", "-c", rowEstimateSQL}
	out, err := e.drv.ExecOutput(ctx, cid, cmd)
	if err != nil {
		return nil, err
	}
	rows := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, count, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("unparseable row estimate line %q", line)
		}
		n, err := strconv.ParseInt(count, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unparseable row estimate line %q: %w", line, err)
		}
		rows[name] = max(n, 0)
	}
	return rows, nil
}

// tableDeltas joins both sides' estimates into a sorted union; tables present
// on one side only count 0 on the other.
func tableDeltas(base, branch map[string]int64) []TableDelta {
	names := map[string]bool{}
	for n := range base {
		names[n] = true
	}
	for n := range branch {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]TableDelta, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, TableDelta{
			Table: n, BaseRows: base[n], BranchRows: branch[n], Delta: branch[n] - base[n],
		})
	}
	return out
}
