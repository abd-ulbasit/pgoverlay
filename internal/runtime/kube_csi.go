package runtime

// csi storage strategy for KubeDriver: volumes are PersistentVolumeClaims,
// CloneVolume is a PVC dataSource clone (or VolumeSnapshot + restore when a
// snapshot class is configured), and pods schedule on any node with no extra
// capabilities — the multi-node mode (Phase 5 decision 4).

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// defaultPVCSize sizes pgbranch PVCs when no size is configured. CSI
	// drivers thin-provision, so a roomy default costs nothing up front.
	defaultPVCSize = "10Gi"
	snapshotGroup  = "snapshot.storage.k8s.io"
)

// snapshotGVR addresses VolumeSnapshot objects (external-snapshotter CRD);
// accessed through the dynamic client to avoid a client dependency on the
// snapshot API module.
var snapshotGVR = schema.GroupVersionResource{Group: snapshotGroup, Version: "v1", Resource: "volumesnapshots"}

func parsePVCSize(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("invalid csi volume size %q: %w", s, err)
	}
	return q, nil
}

type csiStorage struct {
	d             *KubeDriver
	storageClass  string
	snapshotClass string // "" = clone via PVC dataSource
	volumeSize    resource.Quantity
}

// nodeName: no pin — PVCs travel with their pods, the scheduler places them.
func (s *csiStorage) nodeName() string { return "" }

// branchSecurityContext: none. CSI branch pods run postgres directly on
// their cloned claim — no in-container overlay mount, no SYS_ADMIN.
func (s *csiStorage) branchSecurityContext() *corev1.SecurityContext { return nil }

// podVolumes maps MountVolume to the named PVC. MountHostPath cannot occur in
// csi mode (the csi cow backend mounts only PVCs and the zfs backend is
// rejected with csi storage at startup), but map it to a hostPath anyway so a
// future combination fails visibly in the pod, not with a nil volume source.
func (s *csiStorage) podVolumes(ms []Mount) ([]corev1.Volume, []corev1.VolumeMount) {
	vols := make([]corev1.Volume, 0, len(ms))
	mounts := make([]corev1.VolumeMount, 0, len(ms))
	for i, m := range ms {
		name := fmt.Sprintf("vol-%d", i)
		src := corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Volume, ReadOnly: m.ReadOnly},
		}
		if m.Kind == MountHostPath {
			t := corev1.HostPathDirectory
			src = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: m.Volume, Type: &t}}
		}
		vols = append(vols, corev1.Volume{Name: name, VolumeSource: src})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: m.Target, ReadOnly: m.ReadOnly})
	}
	return vols, mounts
}

// buildPVC renders a pgbranch PVC. dataSource is nil for empty volumes, a
// PersistentVolumeClaim reference for CSI clones, or a VolumeSnapshot
// reference for snapshot restores.
func buildPVC(namespace, name, storageClass string, size resource.Quantity, labels map[string]string,
	dataSource *corev1.TypedLocalObjectReference) *corev1.PersistentVolumeClaim {
	sc := storageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
			DataSource: dataSource,
		},
	}
}

// snapshotName names the per-clone VolumeSnapshot (snapshot mode only). It is
// owned by — and removed with — the dst volume.
func snapshotName(dst string) string { return dst + "-snap" }

// buildVolumeSnapshot renders the VolumeSnapshot taken of sourcePVC before a
// snapshot-mode clone, as unstructured (CRD; no typed client dependency).
func buildVolumeSnapshot(namespace, name, snapshotClass, sourcePVC string, labels map[string]string) *unstructured.Unstructured {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": snapshotGroup + "/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    l,
		},
		"spec": map[string]any{
			"volumeSnapshotClassName": snapshotClass,
			"source":                  map[string]any{"persistentVolumeClaimName": sourcePVC},
		},
	}}
}

// createVolume provisions an empty PVC. The actual volume binds when its
// first consumer pod schedules (WaitForFirstConsumer classes) — fine, the
// seed helper is always that consumer.
func (s *csiStorage) createVolume(ctx context.Context, name string, labels map[string]string) error {
	pvc := buildPVC(s.d.namespace, name, s.storageClass, s.volumeSize, labels, nil)
	_, err := s.d.cs.CoreV1().PersistentVolumeClaims(s.d.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

// cloneVolume provisions dst as a copy-on-write copy of src: a PVC with
// dataSource src (CSI cloning), or — when a snapshot class is configured — a
// VolumeSnapshot of src restored into dst.
func (s *csiStorage) cloneVolume(ctx context.Context, src, dst string, labels map[string]string) error {
	dataSource := &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: src}
	if s.snapshotClass != "" {
		snap := buildVolumeSnapshot(s.d.namespace, snapshotName(dst), s.snapshotClass, src, labels)
		if _, err := s.d.dyn.Resource(snapshotGVR).Namespace(s.d.namespace).Create(ctx, snap, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create snapshot %s: %w", snapshotName(dst), err)
		}
		group := snapshotGroup
		dataSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: snapshotName(dst)}
	}
	pvc := buildPVC(s.d.namespace, dst, s.storageClass, s.volumeSize, labels, dataSource)
	if _, err := s.d.cs.CoreV1().PersistentVolumeClaims(s.d.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		if s.snapshotClass != "" { // don't leak the snapshot
			s.d.dyn.Resource(snapshotGVR).Namespace(s.d.namespace).Delete(context.WithoutCancel(ctx), snapshotName(dst), metav1.DeleteOptions{})
		}
		return err
	}
	return nil
}

// listVolumes returns the names of every pgbranch-managed PVC in the namespace
// owned by instanceID.
func (s *csiStorage) listVolumes(ctx context.Context, instanceID string) ([]string, error) {
	list, err := s.d.cs.CoreV1().PersistentVolumeClaims(s.d.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "pgbranch.managed=true," + LabelInstance + "=" + instanceID})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, pvc := range list.Items {
		out = append(out, pvc.Name)
	}
	return out, nil
}

// removeVolume deletes the PVC (and, in snapshot mode, the clone's
// VolumeSnapshot) and waits until both are gone so a same-name recreate
// (branch reset) cannot collide with the asynchronous deletion. Idempotent:
// NotFound is success.
func (s *csiStorage) removeVolume(ctx context.Context, name string) error {
	ns := s.d.namespace
	err := s.d.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	gone := func() (bool, error) {
		if _, err := s.d.cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return false, err // nil err = still there
		}
		if s.snapshotClass == "" {
			return true, nil
		}
		if _, err := s.d.dyn.Resource(snapshotGVR).Namespace(ns).Get(ctx, snapshotName(name), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	}
	if s.snapshotClass != "" {
		err := s.d.dyn.Resource(snapshotGVR).Namespace(ns).Delete(ctx, snapshotName(name), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for {
		if done, err := gone(); done {
			return nil
		} else if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for volume %s to be deleted: %w", name, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}
