package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/volume"
)

// rootHelperCalls records the volume-root helpers a DockerDriver runs and
// answers them with out/err.
type rootHelperCalls struct {
	specs []HelperSpec
	out   func(spec HelperSpec) (string, error)
}

func (c *rootHelperCalls) run(_ context.Context, spec HelperSpec) (string, error) {
	c.specs = append(c.specs, spec)
	if c.out == nil {
		return "", nil
	}
	return c.out(spec)
}

func volumeRootDriver(t *testing.T, root string) (*fakeDockerAPI, *DockerDriver, *rootHelperCalls) {
	t.Helper()
	f, d := newFakeDockerAPI(t)
	d.volumeRoot = root
	calls := &rootHelperCalls{}
	d.helper = calls.run
	return f, d, calls
}

func script(spec HelperSpec) string {
	if len(spec.Cmd) != 3 || spec.Cmd[0] != "sh" || spec.Cmd[1] != "-c" {
		return ""
	}
	return spec.Cmd[2]
}

// runRootScript runs a volume-root helper script locally, against dir as the
// mounted root.
func runRootScript(t *testing.T, spec HelperSpec, dir string) string {
	t.Helper()
	c := exec.Command("sh", "-c", strings.ReplaceAll(script(spec), dataRootMountPath, dir))
	c.Env = append(os.Environ(), spec.Env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func TestValidateVolumeRoot(t *testing.T) {
	for _, ok := range []string{"", "/data/pgoverlay", "/mnt/xfs", "/data/pg overlay"} {
		if err := ValidateVolumeRoot(ok); err != nil {
			t.Errorf("ValidateVolumeRoot(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"/", "data/pg", "./data", "/data/pg/", "/data//pg", "/data/../etc", "/data/pg\nx", "C:\\data"} {
		if err := ValidateVolumeRoot(bad); err == nil {
			t.Errorf("ValidateVolumeRoot(%q) accepted", bad)
		}
	}
	if _, err := NewDockerDriver(WithVolumeRoot("relative/dir")); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("NewDockerDriver with a relative root: %v", err)
	}
}

func TestCreateVolumeUnderVolumeRoot(t *testing.T) {
	f, d, calls := volumeRootDriver(t, "/data/pg")
	labels := map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-1"}
	if err := d.CreateVolume(context.Background(), "pgoverlay-br-x-rw", labels); err != nil {
		t.Fatal(err)
	}
	// the directory first, through a helper that mounts the root
	if len(calls.specs) != 1 {
		t.Fatalf("%d helpers, want 1", len(calls.specs))
	}
	h := calls.specs[0]
	if len(h.Mounts) != 1 || h.Mounts[0] != (Mount{Kind: MountHostPath, Volume: "/data/pg", Target: dataRootMountPath}) {
		t.Fatalf("helper mounts %+v", h.Mounts)
	}
	if h.Privileged || h.SysAdmin || h.Image != UtilityImage {
		t.Fatalf("helper %+v", h)
	}
	var marker map[string]string
	for _, e := range h.Env {
		if v, ok := strings.CutPrefix(e, "PGOVERLAY_VOLUME_LABELS="); ok {
			if err := json.Unmarshal([]byte(v), &marker); err != nil {
				t.Fatal(err)
			}
		}
	}
	if marker[LabelInstance] != "inst-1" || marker[LabelVolumeRoot] != "/data/pg" || marker[labelCreated] == "" {
		t.Fatalf("marker labels %v", marker)
	}
	// then a local bind volume over it, labelled with the root it lives in
	v := f.volumes["pgoverlay-br-x-rw"]
	if v.Options["type"] != "none" || v.Options["o"] != "bind" || v.Options["device"] != "/data/pg/pgoverlay-br-x-rw" {
		t.Fatalf("volume options %v", v.Options)
	}
	if v.Labels[LabelVolumeRoot] != "/data/pg" || v.Labels[LabelInstance] != "inst-1" || v.Labels["pgoverlay.managed"] != "true" {
		t.Fatalf("volume labels %v", v.Labels)
	}
	if labels[LabelVolumeRoot] != "" {
		t.Fatal("CreateVolume modified the caller's labels")
	}

	// the script makes the directory with its marker, and empties a
	// leftover directory of the same name instead of adopting its data
	dir := t.TempDir()
	runRootScript(t, h, dir)
	if b, err := os.ReadFile(filepath.Join(dir, "pgoverlay-br-x-rw", volumeLabelsFile)); err != nil || !strings.Contains(string(b), "inst-1") {
		t.Fatalf("marker: %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pgoverlay-br-x-rw", "old-data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runRootScript(t, h, dir); !strings.Contains(out, "pgoverlay-reclaimed") {
		t.Fatalf("second run did not report the reclaimed directory: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "pgoverlay-br-x-rw", "old-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a leftover directory's data survived into the new volume: %v", err)
	}
}

func TestCreateVolumeUnderVolumeRootFailures(t *testing.T) {
	// an existing volume is refused before any directory is touched
	f, d, calls := volumeRootDriver(t, "/data/pg")
	f.volumes["taken"] = volume.Volume{Name: "taken"}
	if err := d.CreateVolume(context.Background(), "taken", nil); !errors.Is(err, ErrVolumeExists) || len(calls.specs) != 0 {
		t.Fatalf("existing volume: %v, %d helpers", err, len(calls.specs))
	}
	// names become directory names: nothing outside the root
	if err := d.CreateVolume(context.Background(), "../etc", nil); err == nil || len(calls.specs) != 0 {
		t.Fatalf("traversal name: %v, %d helpers", err, len(calls.specs))
	}
	// the directory helper fails: no volume
	calls.out = func(HelperSpec) (string, error) { return "", errors.New("bind source path does not exist: /data/pg") }
	if err := d.CreateVolume(context.Background(), "v1", nil); err == nil || !strings.Contains(err.Error(), "/data/pg/v1") {
		t.Fatalf("helper failure: %v", err)
	}
	if _, ok := f.volumes["v1"]; ok {
		t.Fatal("volume created although its directory was not")
	}
	// the volume create fails: the directory goes again
	calls.out, calls.specs = nil, nil
	f.volumeCreateErr = "driver error"
	if err := d.CreateVolume(context.Background(), "v2", nil); err == nil {
		t.Fatal("want the create error")
	}
	if len(calls.specs) != 2 || script(calls.specs[1]) != "rm -rf "+dataRootMountPath+"/v2" {
		t.Fatalf("helpers %+v, want the directory removed after the failed create", calls.specs)
	}
}

func TestRemoveVolumeUnderVolumeRoot(t *testing.T) {
	f, d, calls := volumeRootDriver(t, "/data/pg")
	if err := d.CreateVolume(context.Background(), "v1", nil); err != nil {
		t.Fatal(err)
	}
	// the configured root changed since: the directory is deleted under the
	// root the volume was created in
	d.volumeRoot = "/elsewhere"
	calls.specs = nil
	if err := d.RemoveVolume(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.volumes["v1"]; ok {
		t.Fatal("volume survived")
	}
	if len(calls.specs) != 1 || script(calls.specs[0]) != "rm -rf "+dataRootMountPath+"/v1" || calls.specs[0].Mounts[0].Volume != "/data/pg" {
		t.Fatalf("helpers %+v, want rm -rf of v1 under /data/pg", calls.specs)
	}

	// the rm script empties exactly that directory
	dir := t.TempDir()
	for _, p := range []string{"v1/upper/base", "v10/keep"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runRootScript(t, calls.specs[0], dir)
	if _, err := os.Stat(filepath.Join(dir, "v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v1 survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v10", "keep")); err != nil {
		t.Fatalf("a sibling directory was touched: %v", err)
	}
}

func TestRemoveVolumeUnderVolumeRootKeepsDataInUse(t *testing.T) {
	f, d, calls := volumeRootDriver(t, "/data/pg")
	if err := d.CreateVolume(context.Background(), "v1", nil); err != nil {
		t.Fatal(err)
	}
	calls.specs = nil
	f.volumeRemoveErr = "remove v1: volume is in use - [abc]"
	if err := d.RemoveVolume(context.Background(), "v1"); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("remove in use: %v", err)
	}
	if len(calls.specs) != 0 {
		t.Fatalf("the directory of a volume in use was touched: %+v", calls.specs)
	}
}

func TestRemoveVolumeHelpersOnlyForRootVolumes(t *testing.T) {
	// a docker-managed volume (created before --volume-root) needs no helper
	f, d, calls := volumeRootDriver(t, "/data/pg")
	f.volumes["plain"] = volume.Volume{Name: "plain", Driver: "local"}
	if err := d.RemoveVolume(context.Background(), "plain"); err != nil || len(calls.specs) != 0 {
		t.Fatalf("plain volume: %v, helpers %+v", err, calls.specs)
	}
	// a missing volume under a root: its directory may be left; delete it
	if err := d.RemoveVolume(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if len(calls.specs) != 1 || script(calls.specs[0]) != "rm -rf "+dataRootMountPath+"/gone" {
		t.Fatalf("missing volume: helpers %+v", calls.specs)
	}
	// without a root, removing a missing volume does nothing
	_, d2, calls2 := volumeRootDriver(t, "")
	if err := d2.RemoveVolume(context.Background(), "gone"); err != nil || len(calls2.specs) != 0 {
		t.Fatalf("no root: %v, helpers %+v", err, calls2.specs)
	}
}

func TestRootVolumeDirNeedsAnExactMatch(t *testing.T) {
	good := volume.Volume{Name: "v1", Driver: "local",
		Labels:  map[string]string{LabelVolumeRoot: "/data/pg"},
		Options: map[string]string{"type": "none", "o": "bind", "device": "/data/pg/v1"}}
	if root, ok := rootVolumeDir(good); !ok || root != "/data/pg" {
		t.Fatalf("good volume: %q %v", root, ok)
	}
	mut := func(f func(v *volume.Volume)) volume.Volume {
		v := good
		v.Labels = map[string]string{LabelVolumeRoot: "/data/pg"}
		v.Options = map[string]string{"type": "none", "o": "bind", "device": "/data/pg/v1"}
		f(&v)
		return v
	}
	for name, v := range map[string]volume.Volume{
		"no label":         mut(func(v *volume.Volume) { delete(v.Labels, LabelVolumeRoot) }),
		"relative root":    mut(func(v *volume.Volume) { v.Labels[LabelVolumeRoot] = "data/pg" }),
		"root is /":        mut(func(v *volume.Volume) { v.Labels[LabelVolumeRoot] = "/"; v.Options["device"] = "/v1" }),
		"other driver":     mut(func(v *volume.Volume) { v.Driver = "rexray" }),
		"not bind":         mut(func(v *volume.Volume) { v.Options["o"] = "rw" }),
		"device elsewhere": mut(func(v *volume.Volume) { v.Options["device"] = "/data/pg/other" }),
		"device is root":   mut(func(v *volume.Volume) { v.Options["device"] = "/data/pg" }),
		"bad name":         mut(func(v *volume.Volume) { v.Name = "../v1"; v.Options["device"] = "/data/v1" }),
	} {
		if _, ok := rootVolumeDir(v); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := rootVolumeDir(mut(func(v *volume.Volume) { v.Options["o"] = "rbind,ro" })); ok {
		t.Error("rbind counted as bind")
	}
	if _, ok := rootVolumeDir(mut(func(v *volume.Volume) { v.Options["o"] = "ro,bind" })); !ok {
		t.Error("bind among other options not recognized")
	}
}

// listing renders listVolumesScript output for the given dirs (name -> label
// file contents, "" for none).
func listing(dirs map[string]string) string {
	var b strings.Builder
	for name, labels := range dirs {
		fmt.Fprintf(&b, "%s\n%s\n%s\n", name, labels, listVolumesSentinel)
	}
	return b.String()
}

func TestListManagedVolumesReportsLeftoverDirectories(t *testing.T) {
	f, d, calls := volumeRootDriver(t, "/data/pg")
	f.volumes["live"] = volume.Volume{Name: "live", Labels: map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-1"}}
	f.volumes["other-labels"] = volume.Volume{Name: "other-labels"}
	calls.out = func(HelperSpec) (string, error) {
		return listing(map[string]string{
			"live":         `{"pgoverlay.instance":"inst-1"}`,
			"left":         `{"pgoverlay.instance":"inst-1","pgoverlay.created":"1700000000"}`,
			"other-labels": `{"pgoverlay.instance":"inst-1"}`, // a volume of that name exists
			"foreign":      `{"pgoverlay.instance":"inst-2"}`,
			"unmarked":     ``,
		}), nil
	}
	vols, err := d.ListManagedVolumes(context.Background(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]VolumeInfo{}
	for _, v := range vols {
		got[v.Name] = v
	}
	if len(got) != 2 || got["live"].Leftover || !got["left"].Leftover || got["left"].Created.Unix() != 1700000000 {
		t.Fatalf("volumes %+v, want live and the leftover dir only", vols)
	}
	if len(calls.specs) != 1 || script(calls.specs[0]) != listVolumesScript(dataRootMountPath) {
		t.Fatalf("helpers %+v", calls.specs)
	}

	// a failing listing fails the call rather than hiding leftovers
	calls.out = func(HelperSpec) (string, error) { return "", errors.New("daemon gone") }
	if _, err := d.ListManagedVolumes(context.Background(), "inst-1"); err == nil {
		t.Fatal("want the listing error")
	}
	// without a root there is nothing to list
	_, d2, calls2 := volumeRootDriver(t, "")
	if _, err := d2.ListManagedVolumes(context.Background(), "inst-1"); err != nil || len(calls2.specs) != 0 {
		t.Fatalf("no root: %v, helpers %d", err, len(calls2.specs))
	}
}

func TestCheckVolumeRoot(t *testing.T) {
	_, d, calls := volumeRootDriver(t, "/data/pg")
	if err := d.CheckVolumeRoot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls.specs) != 1 || calls.specs[0].Mounts[0].Volume != "/data/pg" || calls.specs[0].Mounts[0].Kind != MountHostPath {
		t.Fatalf("helpers %+v", calls.specs)
	}
	runRootScript(t, calls.specs[0], t.TempDir())
	calls.out = func(HelperSpec) (string, error) {
		return "", errors.New(`invalid mount config for type "bind": bind source path does not exist: /data/pg`)
	}
	if err := d.CheckVolumeRoot(context.Background()); err == nil || !strings.Contains(err.Error(), "never created for you") {
		t.Fatalf("missing root: %v", err)
	}
	_, d2, calls2 := volumeRootDriver(t, "")
	if err := d2.CheckVolumeRoot(context.Background()); err != nil || len(calls2.specs) != 0 {
		t.Fatalf("no root: %v, %d helpers", err, len(calls2.specs))
	}
}
