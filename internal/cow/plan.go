// Package cow plans copy-on-write layer layouts for branch containers.
// The overlay mount itself is performed inside the branch container by
// EntrypointScript; host code only decides volume names and mount paths.
package cow

import (
	_ "embed"
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

func SourceVolumeName(source string) string   { return "pgbranch-src-" + source }
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
