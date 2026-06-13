package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func testCSIStorage(snapshotClass string) (*csiStorage, *fake.Clientset, *dynfake.FakeDynamicClient) {
	cs := fake.NewClientset()
	sch := kruntime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(sch,
		map[schema.GroupVersionResource]string{
			{Group: snapshotGroup, Version: "v1", Resource: "volumesnapshots"}: "VolumeSnapshotList",
		})
	d := &KubeDriver{cs: cs, dyn: dyn, namespace: "pgb"}
	s := &csiStorage{d: d, storageClass: "fast-clone", snapshotClass: snapshotClass, volumeSize: resource.MustParse("10Gi")}
	d.storage = s
	return s, cs, dyn
}

func TestBuildPVC(t *testing.T) {
	labels := map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": "main"}
	pvc := buildPVC("pgb", "pgbranch-src-main", "fast-clone", resource.MustParse("10Gi"), labels, nil)
	if pvc.Name != "pgbranch-src-main" || pvc.Namespace != "pgb" {
		t.Errorf("name/ns = %q/%q", pvc.Name, pvc.Namespace)
	}
	if pvc.Labels["pgbranch.managed"] != "true" {
		t.Errorf("labels = %v", pvc.Labels)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-clone" {
		t.Errorf("storageClassName = %v", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v", pvc.Spec.AccessModes)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "10Gi" {
		t.Errorf("storage request = %v", q)
	}
	if pvc.Spec.DataSource != nil {
		t.Errorf("empty volume must have no dataSource, got %+v", pvc.Spec.DataSource)
	}
}

// PVC clone variant: dataSource references the source PVC (CSI cloning).
func TestBuildPVCCloneDataSource(t *testing.T) {
	ds := &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "pgbranch-src-main"}
	pvc := buildPVC("pgb", "pgbranch-br-pr-1-rw", "fast-clone", resource.MustParse("10Gi"), nil, ds)
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Kind != "PersistentVolumeClaim" ||
		pvc.Spec.DataSource.Name != "pgbranch-src-main" {
		t.Fatalf("dataSource = %+v", pvc.Spec.DataSource)
	}
	if pvc.Spec.DataSource.APIGroup != nil {
		t.Errorf("PVC dataSource must have nil apiGroup (core), got %q", *pvc.Spec.DataSource.APIGroup)
	}
}

// csiStorage.cloneVolume without a snapshot class creates exactly one PVC
// whose dataSource is the source claim.
func TestCSICloneVolumePVCDataSource(t *testing.T) {
	s, cs, dyn := testCSIStorage("")
	ctx := context.Background()
	if err := s.cloneVolume(ctx, "pgbranch-src-main", "pgbranch-br-pr-1-rw", map[string]string{"pgbranch.branch.id": "b1"}); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("pgb").Get(ctx, "pgbranch-br-pr-1-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Kind != "PersistentVolumeClaim" || pvc.Spec.DataSource.Name != "pgbranch-src-main" {
		t.Fatalf("dataSource = %+v", pvc.Spec.DataSource)
	}
	if pvc.Labels["pgbranch.branch.id"] != "b1" {
		t.Errorf("labels = %v", pvc.Labels)
	}
	if snaps, err := dyn.Resource(snapshotGVR).Namespace("pgb").List(ctx, metav1.ListOptions{}); err != nil || len(snaps.Items) != 0 {
		t.Errorf("no VolumeSnapshot expected without a snapshot class: %v %v", snaps, err)
	}
}

// snapshot variant: a VolumeSnapshot of the source is created and the new PVC
// restores from it (dataSource kind VolumeSnapshot, apiGroup snapshot.storage.k8s.io).
func TestCSICloneVolumeSnapshotDataSource(t *testing.T) {
	s, cs, dyn := testCSIStorage("csi-snap-class")
	ctx := context.Background()
	if err := s.cloneVolume(ctx, "pgbranch-src-main", "pgbranch-br-pr-1-rw", nil); err != nil {
		t.Fatal(err)
	}
	snap, err := dyn.Resource(snapshotGVR).Namespace("pgb").Get(ctx, "pgbranch-br-pr-1-rw-snap", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("VolumeSnapshot not created: %v", err)
	}
	class, _, _ := unstructured.NestedString(snap.Object, "spec", "volumeSnapshotClassName")
	srcPVC, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
	if class != "csi-snap-class" || srcPVC != "pgbranch-src-main" {
		t.Errorf("snapshot spec: class=%q source=%q", class, srcPVC)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("pgb").Get(ctx, "pgbranch-br-pr-1-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds := pvc.Spec.DataSource
	if ds == nil || ds.Kind != "VolumeSnapshot" || ds.Name != "pgbranch-br-pr-1-rw-snap" ||
		ds.APIGroup == nil || *ds.APIGroup != "snapshot.storage.k8s.io" {
		t.Fatalf("dataSource = %+v", ds)
	}
}

// removeVolume deletes the PVC (and the clone's snapshot in snapshot mode)
// and is idempotent on missing objects.
func TestCSIRemoveVolume(t *testing.T) {
	s, cs, dyn := testCSIStorage("csi-snap-class")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.cloneVolume(ctx, "pgbranch-src-main", "pgbranch-br-pr-1-rw", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.removeVolume(ctx, "pgbranch-br-pr-1-rw"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("pgb").Get(ctx, "pgbranch-br-pr-1-rw", metav1.GetOptions{}); err == nil {
		t.Error("PVC still present after removeVolume")
	}
	if _, err := dyn.Resource(snapshotGVR).Namespace("pgb").Get(ctx, "pgbranch-br-pr-1-rw-snap", metav1.GetOptions{}); err == nil {
		t.Error("VolumeSnapshot still present after removeVolume")
	}
	// idempotent: nothing left to delete
	if err := s.removeVolume(ctx, "pgbranch-br-pr-1-rw"); err != nil {
		t.Errorf("second removeVolume = %v, want nil", err)
	}
	// plain mode driver is idempotent too
	s2, _, _ := testCSIStorage("")
	if err := s2.removeVolume(ctx, "never-existed"); err != nil {
		t.Errorf("removeVolume(missing) = %v, want nil", err)
	}
}

// createVolume renders a plain PVC with the configured class/size.
func TestCSICreateVolume(t *testing.T) {
	s, cs, _ := testCSIStorage("")
	ctx := context.Background()
	if err := s.createVolume(ctx, "pgbranch-src-main", map[string]string{"pgbranch.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("pgb").Get(ctx, "pgbranch-src-main", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource != nil || *pvc.Spec.StorageClassName != "fast-clone" {
		t.Errorf("pvc spec = %+v", pvc.Spec)
	}
}

// Branch pods in csi mode: PVC mounts, NO nodeName pin, NO SYS_ADMIN —
// the multi-node payoff.
func TestBuildBranchPodCSI(t *testing.T) {
	s, _, _ := testCSIStorage("")
	spec := BranchSpec{
		Name:       "pgbranch-br-pr-1",
		Image:      "postgres:17",
		Env:        []string{"PGDATA=/pgbranch/rw/data"},
		Mounts:     []Mount{{Volume: "pgbranch-br-pr-1-rw", Target: "/pgbranch/rw"}},
		Entrypoint: []string{"/bin/sh", "/pgbranch/rw/entrypoint.sh"},
		Labels:     map[string]string{"pgbranch.managed": "true", "pgbranch.role": "branch"},
	}
	pod := buildBranchPod("pgb", s, spec)
	if pod.Spec.NodeName != "" {
		t.Errorf("NodeName = %q, want unpinned", pod.Spec.NodeName)
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext != nil {
		t.Errorf("SecurityContext = %+v, want none (no SYS_ADMIN in csi mode)", c.SecurityContext)
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("volumes = %d", len(pod.Spec.Volumes))
	}
	v := pod.Spec.Volumes[0]
	if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "pgbranch-br-pr-1-rw" {
		t.Fatalf("volume source = %+v, want PVC pgbranch-br-pr-1-rw", v.VolumeSource)
	}
	if m := c.VolumeMounts[0]; m.MountPath != "/pgbranch/rw" || m.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "PGDATA" || c.Env[0].Value != "/pgbranch/rw/data" {
		t.Errorf("Env = %v", c.Env)
	}
}

// Helper pods in csi mode are unpinned and mount PVCs (read-only respected).
func TestBuildHelperPodCSI(t *testing.T) {
	s, _, _ := testCSIStorage("")
	pod := buildHelperPod("pgb", s, HelperSpec{
		Image: "postgres:17",
		Cmd:   []string{"pg_basebackup"},
		User:  "postgres",
		Mounts: []Mount{
			{Volume: "pgbranch-src-main", Target: "/seed"},
			{Volume: "other", Target: "/ro", ReadOnly: true},
		},
	})
	if pod.Spec.NodeName != "" {
		t.Errorf("NodeName = %q, want unpinned", pod.Spec.NodeName)
	}
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("volumes = %d", len(pod.Spec.Volumes))
	}
	if pvc := pod.Spec.Volumes[0].PersistentVolumeClaim; pvc == nil || pvc.ClaimName != "pgbranch-src-main" || pvc.ReadOnly {
		t.Errorf("volume[0] = %+v", pod.Spec.Volumes[0].VolumeSource)
	}
	if pvc := pod.Spec.Volumes[1].PersistentVolumeClaim; pvc == nil || !pvc.ReadOnly {
		t.Errorf("volume[1] = %+v, want read-only PVC", pod.Spec.Volumes[1].VolumeSource)
	}
	if m := pod.Spec.Containers[0].VolumeMounts[1]; !m.ReadOnly {
		t.Errorf("mount[1] = %+v, want read-only", m)
	}
	// helper user mapping still applies in csi mode
	if sc := pod.Spec.Containers[0].SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 999 {
		t.Errorf("SecurityContext = %+v, want RunAsUser 999", pod.Spec.Containers[0].SecurityContext)
	}
}

// The hostPath strategy must keep pinning and SYS_ADMIN (zero regression).
func TestStrategySelection(t *testing.T) {
	hp := &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgbranch"}
	if hp.nodeName() != "node-1" {
		t.Errorf("hostPath nodeName = %q", hp.nodeName())
	}
	sc := hp.branchSecurityContext()
	if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "SYS_ADMIN" {
		t.Errorf("hostPath branch security = %+v, want SYS_ADMIN", sc)
	}
	// newer kernels (fd-based mount API) need the mount syscalls unblocked:
	// unconfined seccomp + AppArmor, matching the docker driver's posture.
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("hostPath branch seccomp = %+v, want Unconfined", sc.SeccompProfile)
	}
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("hostPath branch AppArmor = %+v, want Unconfined", sc.AppArmorProfile)
	}
	csi, _, _ := testCSIStorage("")
	if csi.nodeName() != "" || csi.branchSecurityContext() != nil {
		t.Errorf("csi strategy must not pin nodes or add capabilities")
	}
}

// KubeDriver.CloneVolume on the hostPath strategy runs a root helper that
// cp -a's the source dir and stamps fresh labels.
func TestKubeHostPathCloneVolume(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodSucceeded)
	if err := d.CloneVolume(ctx, "pgbranch-src-main", "pgbranch-br-pr-1-rw", map[string]string{"pgbranch.branch.id": "b1"}); err != nil {
		t.Fatal(err)
	}
	// the helper pod is deleted after completion; assert on the recorded create
	var script string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "pods" {
			continue
		}
		pod := a.(interface{ GetObject() kruntime.Object }).GetObject().(*corev1.Pod)
		if len(pod.Spec.Containers) == 1 && len(pod.Spec.Containers[0].Command) == 3 {
			script = pod.Spec.Containers[0].Command[2]
		}
	}
	for _, want := range []string{"cp -a /pgbranch-root/pgbranch-src-main/. /pgbranch-root/pgbranch-br-pr-1-rw/", ".pgbranch-labels.json"} {
		if !strings.Contains(script, want) {
			t.Errorf("clone helper script %q missing %q", script, want)
		}
	}
}

// CloneVolume validates both names (they become paths / object names).
func TestCloneVolumeRejectsBadNames(t *testing.T) {
	d, _ := fakeKubeDriver(t)
	ctx := context.Background()
	if err := d.CloneVolume(ctx, "../etc", "dst", nil); err == nil {
		t.Error("bad src accepted")
	}
	if err := d.CloneVolume(ctx, "src", "a b", nil); err == nil {
		t.Error("bad dst accepted")
	}
}
