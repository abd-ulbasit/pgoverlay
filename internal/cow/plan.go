// Package cow plans copy-on-write layer layouts for branch containers.
// The overlay mount itself is performed inside the branch container by
// EntrypointScript; host code only decides volume names and mount paths.
package cow

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed entrypoint.sh
var EntrypointScript string

const (
	MergedPath = "/pgbranch/merged" // PGDATA inside branch container
	RWPath     = "/pgbranch/rw"     // branch rw volume mountpoint
)

type Plan struct {
	SourceVolume string   // mounted ro at /pgbranch/lower0
	RWVolume     string   // upper+work live here
	Lowers       []string // in overlay order, topmost first
}

// SourceVolumeName names a source's seed volume for a given generation.
// Generation 1 keeps the legacy Phase 1 name so existing volumes keep working.
func SourceVolumeName(source string, gen int) string {
	if gen <= 1 {
		return "pgbranch-src-" + source
	}
	return fmt.Sprintf("pgbranch-src-%s-g%d", source, gen)
}
func BranchRWVolumeName(branch string) string { return "pgbranch-br-" + branch + "-rw" }

func PlanBranch(branchName, sourceVolume string) Plan {
	return Plan{
		SourceVolume: sourceVolume,
		RWVolume:     BranchRWVolumeName(branchName),
		// Seeding writes the cluster into a data/ subdir of the source volume
		// (pg_basebackup creates it 0700-owned by uid 999), so the overlay
		// lower is <mountpoint>/data.
		Lowers: []string{"/pgbranch/lower0/data"},
	}
}

// LowerEnv renders PGBRANCH_LOWERS for the entrypoint (colon-separated).
func (p Plan) LowerEnv() string { return strings.Join(p.Lowers, ":") }
