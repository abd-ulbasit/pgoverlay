package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// KubeDriver runs branches as pods. Where the data lives is a pluggable
// storage strategy:
//
//   - hostPath (default): "volumes" are subdirectories of dataRoot on one
//     designated storage node; every pod is pinned there with nodeName and
//     branch pods get SYS_ADMIN for their in-container overlay mount
//     (decision 1: single-node dev/test scope).
//   - csi: "volumes" are PersistentVolumeClaims and branches are PVC clones;
//     pods schedule anywhere, need no extra capabilities, and run postgres
//     directly on their claim (multi-node scope, decision 4 / Phase 5 D).
//
// Container IDs are pod names.
type KubeDriver struct {
	cs        kubernetes.Interface
	dyn       dynamic.Interface // VolumeSnapshot ops (csi snapshot mode); nil otherwise
	cfg       *rest.Config      // for exec (SPDY); nil only in unit tests
	namespace string
	storage   kubeStorage
}

// kubeStorage is the storage strategy inside KubeDriver: it owns volume
// provisioning and decides how driver mounts and pod placement/privileges
// translate to pod specs.
type kubeStorage interface {
	createVolume(ctx context.Context, name string, labels map[string]string) error
	removeVolume(ctx context.Context, name string) error
	cloneVolume(ctx context.Context, src, dst string, labels map[string]string) error
	// listVolumes returns the names of every pgbranch-managed volume owned by
	// instanceID (hostPath: dirs under the data root whose .pgbranch-labels.json
	// carries the id; csi: PVCs labelled pgbranch.managed=true,pgbranch.instance=<id>).
	listVolumes(ctx context.Context, instanceID string) ([]string, error)
	// podVolumes translates driver mounts to pod volumes + container mounts.
	podVolumes(ms []Mount) ([]corev1.Volume, []corev1.VolumeMount)
	// nodeName pins pods to the storage node ("" = let the scheduler place).
	nodeName() string
	// branchSecurityContext is the branch container's security context
	// (SYS_ADMIN for in-container overlay mounts; nil = none needed).
	branchSecurityContext() *corev1.SecurityContext
}

const volumeHelperImage = "alpine:3.21"

// kubeRestConfig loads the cluster config: kubeconfig=="" uses in-cluster
// config when available, else the default kubeconfig loading rules
// (KUBECONFIG / ~/.kube/config).
func kubeRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err == rest.ErrNotInCluster {
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	}
	return cfg, err
}

// NewKubeClient builds a kubernetes.Interface from the same in-cluster /
// kubeconfig loading path the kube driver uses (kubeconfig=="" → in-cluster,
// then KUBECONFIG / ~/.kube/config). branchd reuses it for the leader-election
// Lease so HA shares the driver's cluster credentials.
func NewKubeClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := kubeRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	return cs, nil
}

// NewKubeDriver connects to the cluster with the hostPath storage strategy
// (all data under dataRoot on the named storage node).
func NewKubeDriver(kubeconfig, namespace, nodeName, dataRoot string) (*KubeDriver, error) {
	cfg, err := kubeRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	if namespace == "" {
		namespace = "default"
	}
	if dataRoot == "" {
		dataRoot = "/var/lib/pgbranch"
	}
	if nodeName == "" {
		return nil, fmt.Errorf("kube driver requires a storage node name")
	}
	d := &KubeDriver{cs: cs, cfg: cfg, namespace: namespace}
	d.storage = &hostPathStorage{d: d, node: nodeName, dataRoot: dataRoot}
	return d, nil
}

// CSIConfig configures the csi storage strategy.
type CSIConfig struct {
	// StorageClass provisions every pgbranch PVC; it must support PVC
	// dataSource cloning (or VolumeSnapshots when SnapshotClass is set).
	StorageClass string
	// SnapshotClass switches branch cloning from PVC dataSource clones to
	// VolumeSnapshot + restore ("" = direct PVC clones).
	SnapshotClass string
	// VolumeSize is the storage request of every pgbranch PVC ("" = 10Gi).
	VolumeSize string
}

// NewKubeDriverCSI connects to the cluster with the csi storage strategy:
// volumes are PVCs, branches are PVC clones, pods schedule on any node.
func NewKubeDriverCSI(kubeconfig, namespace string, csi CSIConfig) (*KubeDriver, error) {
	if csi.StorageClass == "" {
		return nil, fmt.Errorf("csi storage requires a storage class")
	}
	cfg, err := kubeRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	var dyn dynamic.Interface
	if csi.SnapshotClass != "" {
		if dyn, err = dynamic.NewForConfig(cfg); err != nil {
			return nil, fmt.Errorf("kube dynamic client: %w", err)
		}
	}
	if namespace == "" {
		namespace = "default"
	}
	if csi.VolumeSize == "" {
		csi.VolumeSize = defaultPVCSize
	}
	size, err := parsePVCSize(csi.VolumeSize)
	if err != nil {
		return nil, err
	}
	d := &KubeDriver{cs: cs, dyn: dyn, cfg: cfg, namespace: namespace}
	d.storage = &csiStorage{d: d, storageClass: csi.StorageClass, snapshotClass: csi.SnapshotClass, volumeSize: size}
	return d, nil
}

// EnsureImage is a no-op: the kubelet pulls images on pod start.
func (d *KubeDriver) EnsureImage(ctx context.Context, image string) error { return nil }

// CreateVolume provisions an empty volume (hostPath: node dir via helper pod;
// csi: PVC) carrying the given labels.
func (d *KubeDriver) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if err := validVolumeName(name); err != nil {
		return err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if err := d.storage.createVolume(ctx, name, labels); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// ListManagedVolumes returns every pgbranch-managed volume name (delegated to
// the storage strategy: hostPath dirs or labelled PVCs).
func (d *KubeDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]string, error) {
	return d.storage.listVolumes(ctx, instanceID)
}

// RemoveVolume deletes the volume. Idempotent (removing a missing volume
// succeeds).
func (d *KubeDriver) RemoveVolume(ctx context.Context, name string) error {
	if err := validVolumeName(name); err != nil {
		return err
	}
	if err := d.storage.removeVolume(ctx, name); err != nil {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

// CloneVolume provisions dst as a copy of src: a full `cp -a` through a
// helper pod (hostPath) or a copy-on-write PVC clone / snapshot restore (csi).
func (d *KubeDriver) CloneVolume(ctx context.Context, src, dst string, labels map[string]string) error {
	if err := validVolumeName(src); err != nil {
		return err
	}
	if err := validVolumeName(dst); err != nil {
		return err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if err := d.storage.cloneVolume(ctx, src, dst, labels); err != nil {
		return fmt.Errorf("clone volume %s -> %s: %w", src, dst, err)
	}
	return nil
}

// hostPathStorage is the original single-node strategy: volume name ->
// <dataRoot>/<name> on the storage node, pods pinned there via nodeName,
// branch pods overlay-mount in-container (SYS_ADMIN).
type hostPathStorage struct {
	d        *KubeDriver
	node     string
	dataRoot string
}

func (s *hostPathStorage) nodeName() string { return s.node }

// branchSecurityContext: SYS_ADMIN is required for the in-container overlay
// mount. Newer kernels (≥ ~6.7) with util-linux ≥ 2.39 drive mounts through
// the fd-based mount API (fsopen/fsconfig/fsmount/move_mount); the default
// container seccomp/AppArmor profiles block those syscalls, so the overlay
// mount fails with "overlay: No changes allowed in reconfigure" under
// SYS_ADMIN alone (observed on a k3s node, kernel 7.0 / util-linux 2.41).
// Running the branch container with unconfined seccomp and AppArmor — the
// same effective posture as the docker driver's apparmor=unconfined —
// restores it. Branch pods are already a privileged dev/test scope.
func (s *hostPathStorage) branchSecurityContext() *corev1.SecurityContext {
	unconfined := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	return &corev1.SecurityContext{
		Capabilities:    &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
		SeccompProfile:  &unconfined,
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
	}
}

func (s *hostPathStorage) podVolumes(ms []Mount) ([]corev1.Volume, []corev1.VolumeMount) {
	return hostPathPodVolumes(s.dataRoot, ms)
}

// createVolume mkdirs the volume dir on the storage node via a helper pod and
// records the labels in <vol>/.pgbranch-labels.json.
func (s *hostPathStorage) createVolume(ctx context.Context, name string, labels map[string]string) error {
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	dir := dataRootMountPath + "/" + name
	cmd := fmt.Sprintf(`mkdir -p %s && printf '%%s' "$PGBRANCH_VOLUME_LABELS" > %s/%s`, dir, dir, volumeLabelsFile)
	_, err = s.runRootHelper(ctx, cmd, []string{"PGBRANCH_VOLUME_LABELS=" + string(j)})
	return err
}

// removeVolume deletes the volume dir on the storage node (rm -rf on a
// missing dir succeeds).
func (s *hostPathStorage) removeVolume(ctx context.Context, name string) error {
	_, err := s.runRootHelper(ctx, "rm -rf "+dataRootMountPath+"/"+name, nil)
	return err
}

// cloneVolume copies src's directory into a fresh dst dir (full copy — plain
// directories have no CoW primitive) and stamps dst with its own labels.
func (s *hostPathStorage) cloneVolume(ctx context.Context, src, dst string, labels map[string]string) error {
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	srcDir, dstDir := dataRootMountPath+"/"+src, dataRootMountPath+"/"+dst
	cmd := fmt.Sprintf(`rm -rf %s && mkdir -p %s && cp -a %s/. %s/ && printf '%%s' "$PGBRANCH_VOLUME_LABELS" > %s/%s`,
		dstDir, dstDir, srcDir, dstDir, dstDir, volumeLabelsFile)
	_, err = s.runRootHelper(ctx, cmd, []string{"PGBRANCH_VOLUME_LABELS=" + string(j)})
	return err
}

// listVolumes enumerates the volume dirs under the data root and returns only
// those whose .pgbranch-labels.json records pgbranch.instance=<instanceID>.
// Each managed volume dir is emitted on its own line followed by its label
// file's contents on the next (NUL-padded marker lines bracket each entry); a
// dir whose marker is missing or names a different instance is foreign and
// skipped. A missing data root (nothing created yet) lists nothing.
func (s *hostPathStorage) listVolumes(ctx context.Context, instanceID string) ([]string, error) {
	// For every entry under the data root, print "<name>\n", then its label
	// JSON on one line, then a sentinel. Tolerate an empty/missing root.
	script := fmt.Sprintf(
		`for d in %s/*/; do [ -d "$d" ] || continue; n=$(basename "$d"); printf '%%s\n' "$n"; cat "$d/%s" 2>/dev/null; printf '\n%s\n'; done 2>/dev/null || true`,
		dataRootMountPath, volumeLabelsFile, listVolumesSentinel)
	out, err := s.runRootHelper(ctx, script, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range strings.Split(out, listVolumesSentinel) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		lines := strings.SplitN(entry, "\n", 2)
		name := strings.TrimSpace(lines[0])
		if name == "" || name == volumeLabelsFile {
			continue
		}
		var labels map[string]string
		if len(lines) == 2 {
			_ = json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &labels)
		}
		if labels[LabelInstance] == instanceID {
			names = append(names, name)
		}
	}
	return names, nil
}

// listVolumesSentinel brackets each volume entry in the hostPath listVolumes
// helper output so a name can be split cleanly from its (possibly empty) label
// JSON; chosen to never collide with a volume name or JSON content.
const listVolumesSentinel = "\x00--pgbranch-vol--\x00"

// runRootHelper runs sh -c cmd in a helper pod with the whole data root
// mounted at dataRootMountPath (needed to create/remove volume dirs).
func (s *hostPathStorage) runRootHelper(ctx context.Context, cmd string, env []string) (string, error) {
	pod := buildHelperPod(s.d.namespace, s,
		HelperSpec{Image: volumeHelperImage, Cmd: []string{"sh", "-c", cmd}, Env: env})
	t := corev1.HostPathDirectoryOrCreate
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         "data-root",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: s.dataRoot, Type: &t}},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "data-root", MountPath: dataRootMountPath})
	return s.d.runPodToCompletion(ctx, pod)
}

func (d *KubeDriver) RunHelper(ctx context.Context, spec HelperSpec) (string, error) {
	return d.runPodToCompletion(ctx, buildHelperPod(d.namespace, d.storage, spec))
}

// runPodToCompletion creates the pod, waits for Succeeded/Failed (deadline
// from ctx), and always deletes it. The pod's captured logs are returned on
// success and embedded in the error on failure.
func (d *KubeDriver) runPodToCompletion(ctx context.Context, pod *corev1.Pod) (string, error) {
	created, err := d.cs.CoreV1().Pods(d.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		prop := metav1.DeletePropagationBackground
		d.cs.CoreV1().Pods(d.namespace).Delete(context.WithoutCancel(ctx), created.Name,
			metav1.DeleteOptions{PropagationPolicy: &prop})
	}()
	phase, err := d.waitPodDone(ctx, created.Name)
	if err != nil {
		return "", fmt.Errorf("helper pod %s: %w", created.Name, err)
	}
	logs := d.podLogs(ctx, created.Name)
	if phase != corev1.PodSucceeded {
		return logs, fmt.Errorf("helper pod %s failed: %s", created.Name, logs)
	}
	return logs, nil
}

// waitPodDone watches the pod until it reaches a terminal phase. The watch is
// established before the initial Get so no transition can be missed.
func (d *KubeDriver) waitPodDone(ctx context.Context, name string) (corev1.PodPhase, error) {
	w, err := d.cs.CoreV1().Pods(d.namespace).Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return "", fmt.Errorf("watch: %w", err)
	}
	defer w.Stop()
	terminal := func(p corev1.PodPhase) bool { return p == corev1.PodSucceeded || p == corev1.PodFailed }
	if pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, name, metav1.GetOptions{}); err == nil && terminal(pod.Status.Phase) {
		return pod.Status.Phase, nil
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return "", fmt.Errorf("watch closed before pod finished")
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok || pod.Name != name {
				continue
			}
			if terminal(pod.Status.Phase) {
				return pod.Status.Phase, nil
			}
		}
	}
}

func (d *KubeDriver) podLogs(ctx context.Context, name string) string {
	tail := int64(20)
	raw, err := d.cs.CoreV1().Pods(d.namespace).
		GetLogs(name, &corev1.PodLogOptions{TailLines: &tail}).Do(ctx).Raw()
	if err != nil {
		return fmt.Sprintf("(logs unavailable: %v)", err)
	}
	return string(raw)
}

func (d *KubeDriver) StartBranch(ctx context.Context, spec BranchSpec) (string, error) {
	pod := buildBranchPod(d.namespace, d.storage, spec)
	created, err := d.cs.CoreV1().Pods(d.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create branch pod: %w", err)
	}
	return created.Name, nil
}

// Exec runs cmd in the pod's first container and fails on non-zero exit with
// captured output, matching the docker driver's contract.
func (d *KubeDriver) Exec(ctx context.Context, id string, cmd []string) error {
	_, err := d.ExecOutput(ctx, id, cmd)
	return err
}

// ExecOutput runs cmd in the pod's first container over the SPDY exec
// subresource and returns the captured stdout; stderr is kept separate and
// embedded in the error on failure (non-zero exit surfaces as a stream error).
func (d *KubeDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	req := d.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(d.namespace).Name(id).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: pod.Spec.Containers[0].Name,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(d.cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %v: %w: %s%s", cmd, err, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

func (d *KubeDriver) Inspect(ctx context.Context, id string) (ContainerInfo, error) {
	pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return ContainerInfo{}, err
	}
	return podInfo(pod), nil
}

func podInfo(pod *corev1.Pod) ContainerInfo {
	return ContainerInfo{
		ID:      pod.Name,
		Running: pod.Status.Phase == corev1.PodRunning,
		Host:    pod.Status.PodIP,
		Port:    5432,
		Labels:  pod.Labels,
	}
}

// StopRemove deletes the pod (30s grace, background propagation) and waits
// until it is gone so a same-name recreate (branch reset) cannot collide.
// Idempotent: NotFound is success.
func (d *KubeDriver) StopRemove(ctx context.Context, id string) error {
	grace := int64(30)
	prop := metav1.DeletePropagationBackground
	err := d.cs.CoreV1().Pods(d.namespace).Delete(ctx, id,
		metav1.DeleteOptions{GracePeriodSeconds: &grace, PropagationPolicy: &prop})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for {
		if _, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, id, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for pod %s to terminate: %w", id, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (d *KubeDriver) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	pods, err := d.cs.CoreV1().Pods(d.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "pgbranch.managed=true,pgbranch.role=branch"})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, podInfo(&pods.Items[i]))
	}
	return out, nil
}
