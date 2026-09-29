package runtime

// Docker volume root (branchd --volume-root, PGOVERLAY_VOLUME_ROOT).
//
// By default pgoverlay's volumes are plain docker named volumes, stored
// wherever the daemon keeps them (/var/lib/docker/volumes). With a volume root
// DIR, every volume the driver creates is still a named volume, mounted,
// listed, labelled and removed by name exactly as before, but a local-driver
// bind volume over the directory DIR/<name> (driver_opts type=none, o=bind,
// device=DIR/<name>). That puts all branch data (source generations, writable
// layers, frozen layers, diff throwaways) on a filesystem the operator picks:
// on a host whose Docker root is ext4 but which has an XFS (reflink=1) or
// btrfs disk, OverlayFS copy-up becomes an extent clone, which is block-level
// copy-on-write, without moving Docker.
//
// The daemon neither creates nor deletes a bind volume's directory, so the
// driver does, in a helper container that mounts DIR: CreateVolume makes
// DIR/<name> and records the volume's labels in it (.pgoverlay-labels.json,
// the marker the kube hostPath storage keeps too), and RemoveVolume deletes
// the directory once the volume is gone. A directory whose volume is gone
// (a removal that stopped half way, or a `docker volume rm`) is reported by
// ListManagedVolumes as a Leftover, so reconcile's volume GC finds it.
//
// DIR is a path on the Docker host: not this machine when DOCKER_HOST points
// elsewhere, and inside the VM for Docker Desktop, Colima and OrbStack. It
// must exist; the driver never creates it, so a disk that failed to mount
// cannot silently turn into a directory on the root filesystem.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// LabelVolumeRoot records on a volume created under a volume root which root
// that was. RemoveVolume deletes the directory under the root the volume was
// created with, so changing or dropping --volume-root later strands nothing.
const LabelVolumeRoot = "pgoverlay.volume-root"

// DockerOption configures a DockerDriver.
type DockerOption func(*DockerDriver)

// WithVolumeRoot makes every volume the driver creates a bind volume over a
// directory under dir on the Docker host (see the comment at the top of this
// file). "" keeps docker-managed volumes. Volumes that already exist keep
// working either way: they are mounted by name, and removed according to how
// they were created.
func WithVolumeRoot(dir string) DockerOption {
	return func(d *DockerDriver) { d.volumeRoot = dir }
}

// VolumeRoot is the configured volume root ("" = docker-managed volumes).
func (d *DockerDriver) VolumeRoot() string { return d.volumeRoot }

// ValidateVolumeRoot rejects a volume root that cannot be used as one: it
// must be an absolute, clean path on the (Linux) Docker host, other than /.
func ValidateVolumeRoot(dir string) error {
	switch {
	case dir == "":
		return nil
	case !strings.HasPrefix(dir, "/"):
		return fmt.Errorf("volume root %q: must be an absolute path on the Docker host", dir)
	case path.Clean(dir) != dir:
		return fmt.Errorf("volume root %q: must be a clean path (%q)", dir, path.Clean(dir))
	case dir == "/":
		return fmt.Errorf("volume root %q: must be a directory below /", dir)
	case strings.ContainsAny(dir, "\x00\n"):
		return fmt.Errorf("volume root %q: contains a NUL or newline", dir)
	}
	return nil
}

// CheckVolumeRoot verifies that the volume root exists on the Docker host and
// is a writable directory. branchd runs it at startup so a missing disk fails
// there, not in the first branch create. No-op without a volume root.
func (d *DockerDriver) CheckVolumeRoot(ctx context.Context) error {
	if d.volumeRoot == "" {
		return nil
	}
	script := `set -eu
[ -d ` + dataRootMountPath + ` ] || { echo "not a directory" >&2; exit 1; }
t=` + dataRootMountPath + `/.pgoverlay-check-$$
mkdir "$t"
rmdir "$t"`
	if _, err := d.rootHelper(ctx, d.volumeRoot, script, nil); err != nil {
		return fmt.Errorf("volume root %s on the Docker host is not a usable directory (create it first; it is never created for you): %w", d.volumeRoot, err)
	}
	return nil
}

// createRootVolume makes <root>/<name> and a local-driver bind volume over
// it. A directory already there has no volume (CreateVolume checked), so it
// is what an interrupted removal of an earlier volume of that name left
// behind: it is emptied rather than handed to the new volume, which must
// start empty.
func (d *DockerDriver) createRootVolume(ctx context.Context, name string, labels map[string]string) error {
	if err := validVolumeName(name); err != nil {
		return fmt.Errorf("create volume: %w", err)
	}
	lbl := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		lbl[k] = v
	}
	lbl[LabelVolumeRoot] = d.volumeRoot
	marker, err := json.Marshal(withCreatedLabel(lbl, time.Now()))
	if err != nil {
		return err
	}
	dir := path.Join(d.volumeRoot, name)
	script := fmt.Sprintf(`set -eu
d=%s/%s
if [ -e "$d" ] || [ -L "$d" ]; then rm -rf "$d"; echo pgoverlay-reclaimed; fi
mkdir "$d"
printf '%%s' "$PGOVERLAY_VOLUME_LABELS" > "$d/%s"`, dataRootMountPath, name, volumeLabelsFile)
	out, err := d.rootHelper(ctx, d.volumeRoot, script, []string{"PGOVERLAY_VOLUME_LABELS=" + string(marker)})
	if err != nil {
		return fmt.Errorf("create volume %s: make %s: %w", name, dir, err)
	}
	if strings.Contains(out, "pgoverlay-reclaimed") {
		slog.Warn("create volume: a directory of that name was left under the volume root without a volume; emptied it for the new volume",
			"volume", name, "dir", dir)
	}
	_, err = d.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:       name,
		Driver:     "local",
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": dir},
		Labels:     lbl,
	})
	if err != nil {
		if rmErr := d.removeRootDir(context.WithoutCancel(ctx), d.volumeRoot, name); rmErr != nil {
			slog.Warn("create volume: remove the directory of a volume that could not be created", "volume", name, "dir", dir, "err", rmErr)
		}
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// rootVolumeDir reports whether v is a bind volume this driver created under
// a volume root, and which root. Only an exact match counts (local driver,
// bind option, device <root>/<name> for the root its label records), so
// RemoveVolume never deletes a directory it did not create.
func rootVolumeDir(v volume.Volume) (string, bool) {
	root := v.Labels[LabelVolumeRoot]
	if root == "" || ValidateVolumeRoot(root) != nil || validVolumeName(v.Name) != nil {
		return "", false
	}
	if v.Driver != "" && v.Driver != "local" {
		return "", false
	}
	bind := false
	for _, o := range strings.Split(v.Options["o"], ",") {
		if strings.TrimSpace(o) == "bind" {
			bind = true
		}
	}
	if !bind || v.Options["device"] != path.Join(root, v.Name) {
		return "", false
	}
	return root, true
}

// removeRootDir deletes <root>/<name>. Idempotent: a missing directory is
// success.
func (d *DockerDriver) removeRootDir(ctx context.Context, root, name string) error {
	if err := validVolumeName(name); err != nil {
		return err
	}
	if _, err := d.rootHelper(ctx, root, "rm -rf "+dataRootMountPath+"/"+name, nil); err != nil {
		return fmt.Errorf("remove volume %s: delete %s: %w", name, path.Join(root, name), err)
	}
	return nil
}

// leftoverRootDirs lists the directories under the volume root that carry
// instanceID's marker but have no volume: storage only garbage collection
// should see (VolumeInfo.Leftover). have is the instance's volumes as the
// daemon lists them.
func (d *DockerDriver) leftoverRootDirs(ctx context.Context, instanceID string, have []VolumeInfo) ([]VolumeInfo, error) {
	out, err := d.rootHelper(ctx, d.volumeRoot, listVolumesScript(dataRootMountPath), nil)
	if err != nil {
		return nil, fmt.Errorf("list volume root %s: %w", d.volumeRoot, err)
	}
	known := make(map[string]bool, len(have))
	for _, v := range have {
		known[v.Name] = true
	}
	var left []VolumeInfo
	for _, v := range parseVolumeList(out, instanceID) {
		if known[v.Name] || validVolumeName(v.Name) != nil {
			continue
		}
		// the daemon's list is filtered by label; make sure no volume of
		// that name exists at all before calling its directory a leftover
		if _, err := d.cli.VolumeInspect(ctx, v.Name); err == nil {
			continue
		} else if !client.IsErrNotFound(err) {
			return nil, fmt.Errorf("list volume root %s: inspect %s: %w", d.volumeRoot, v.Name, err)
		}
		v.Leftover = true
		left = append(left, v)
	}
	return left, nil
}

// rootHelper runs sh -c script in a helper with root (a directory on the
// Docker host) mounted at dataRootMountPath.
func (d *DockerDriver) rootHelper(ctx context.Context, root, script string, env []string) (string, error) {
	if root == "" {
		return "", errors.New("no volume root")
	}
	spec := HelperSpec{
		Image:  UtilityImage,
		Cmd:    []string{"sh", "-c", script},
		Env:    env,
		Mounts: []Mount{{Kind: MountHostPath, Volume: root, Target: dataRootMountPath}},
	}
	if d.helper != nil {
		return d.helper(ctx, spec)
	}
	return d.RunHelper(ctx, spec)
}
