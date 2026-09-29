package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// copyUpProbeTimeout bounds the startup copy-up probe: two volume creates, a
// helper that writes and copies up 64 MiB, two removals (helper pods on kube).
const copyUpProbeTimeout = 5 * time.Minute

// checkVolumeRootFlag validates --volume-root against the runtime and
// backend it applies to: the docker runtime, whose volumes it relocates, and
// a backend that uses driver volumes (overlay; zfs keeps its data in
// datasets). kube hostPath has --kube-data-root for the same purpose.
func checkVolumeRootFlag(root, runtimeName string, backend cow.Backend) error {
	if root == "" {
		return nil
	}
	if runtimeName != "docker" {
		return errors.New("--volume-root (PGOVERLAY_VOLUME_ROOT) applies to the docker runtime; with --runtime kube, --kube-data-root places the data")
	}
	if backend != cow.BackendOverlay {
		return fmt.Errorf("--volume-root (PGOVERLAY_VOLUME_ROOT) applies to the overlay backend; --cow %s keeps its data elsewhere", backend)
	}
	return runtime.ValidateVolumeRoot(root)
}

// parseByteSize reads a byte count with an optional k or m suffix (KiB,
// MiB): "16k" is 16384, "0" is 0.
func parseByteSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "k"):
		mult, t = 1<<10, strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		mult, t = 1<<20, strings.TrimSuffix(t, "m")
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 || n > (1<<31)/mult {
		return 0, fmt.Errorf("%q is not a byte count (for example 16k, 1m or 0)", s)
	}
	return n * mult, nil
}

// diskRootOverride is the --disk-root value storageRoot gets: the flag when
// set, else the docker volume root when it is a directory on this machine
// (branchd runs on the Docker host), since that is where branch data lives.
// A volume root on a remote Docker host cannot be measured from here.
func diskRootOverride(flagValue, volumeRoot string, isDir func(string) bool) string {
	if flagValue != "" {
		return flagValue
	}
	if volumeRoot != "" && isDir(volumeRoot) {
		return volumeRoot
	}
	return ""
}

func isLocalDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// probeRoot is the host directory volumes are created under, where the
// copy-up probe may set the XFS copy-on-write extent size hint: the docker
// volume root, or the kube hostPath data root. "" for docker-managed
// volumes, whose directories belong to Docker.
func probeRoot(runtimeName, volumeRoot, kubeDataRoot string) string {
	if runtimeName == "kube" {
		return kubeDataRoot
	}
	return volumeRoot
}
