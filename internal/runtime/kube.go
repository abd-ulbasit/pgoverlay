package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/watch"
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

	// helperImage replaces UtilityImage in helper pods ("" = UtilityImage).
	helperImage string
	// instanceID labels helper pods and their Secrets (LabelInstance) so
	// orphan GC can tell this registry's helpers from another's ("" = none).
	instanceID string
	// owner is branchd's own pod, made the owner of every helper pod and
	// helper Secret (nil = none, e.g. branchd running outside the cluster).
	owner *metav1.OwnerReference
	// helperStartTimeout bounds how long a helper pod may take to start its
	// container (0 = defaultHelperStartTimeout).
	helperStartTimeout time.Duration
}

// KubeOption configures a KubeDriver beyond its storage strategy.
type KubeOption func(*KubeDriver)

// WithHelperImage runs every helper that asks for UtilityImage on image
// instead (branchd --kube-helper-image): a mirror in a private or air-gapped
// registry, or a newer pinned digest. "" keeps UtilityImage.
func WithHelperImage(image string) KubeOption {
	return func(d *KubeDriver) { d.helperImage = image }
}

// WithInstanceID stamps LabelInstance=id on helper pods and their Secrets,
// the same instance label branch pods and volumes carry, so a helper left
// behind by a crashed branchd can be attributed to the registry that ran it.
func WithInstanceID(id string) KubeOption {
	return func(d *KubeDriver) { d.instanceID = id }
}

// WithOwnerPod makes branchd's own pod the owner of every helper pod and
// helper Secret, so Kubernetes garbage collection removes them once that pod
// is gone: a branchd killed mid-seed (a crash, a rollout past the shutdown
// budget) would otherwise leave its helper running, and its Secret stored,
// with nothing to clean them up. namespace, name and uid come from the
// downward API. It is ignored unless all three are set and namespace is the
// driver's: an owner in another namespace counts as missing, which would get
// every helper collected the moment it is created.
func WithOwnerPod(namespace, name, uid string) KubeOption {
	return func(d *KubeDriver) {
		if namespace == "" || name == "" || uid == "" || namespace != d.namespace {
			return
		}
		d.owner = &metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: name, UID: types.UID(uid)}
	}
}

// kubeStorage is the storage strategy inside KubeDriver: it owns volume
// provisioning and decides how driver mounts and pod placement/privileges
// translate to pod specs.
type kubeStorage interface {
	createVolume(ctx context.Context, name string, labels map[string]string) error
	removeVolume(ctx context.Context, name string) error
	cloneVolume(ctx context.Context, src, dst string, labels map[string]string) error
	// listVolumes returns every pgoverlay-managed volume owned by instanceID
	// (hostPath: dirs under the data root whose .pgoverlay-labels.json carries
	// the id; csi: PVCs labelled pgoverlay.managed=true,pgoverlay.instance=<id>).
	listVolumes(ctx context.Context, instanceID string) ([]VolumeInfo, error)
	// podVolumes translates driver mounts to pod volumes + container mounts.
	podVolumes(ms []Mount) ([]corev1.Volume, []corev1.VolumeMount)
	// nodeName pins pods to the storage node ("" = let the scheduler place).
	nodeName() string
	// branchSecurityContext is the branch container's security context
	// (hostPath: SYS_ADMIN + unconfined for in-container overlay mounts; csi:
	// RuntimeDefault seccomp, no privilege escalation).
	branchSecurityContext() *corev1.SecurityContext
}

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
func NewKubeDriver(kubeconfig, namespace, nodeName, dataRoot string, opts ...KubeOption) (*KubeDriver, error) {
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
		dataRoot = "/var/lib/pgoverlay"
	}
	if nodeName == "" {
		return nil, fmt.Errorf("kube driver requires a storage node name")
	}
	d := &KubeDriver{cs: cs, cfg: cfg, namespace: namespace}
	d.storage = &hostPathStorage{d: d, node: nodeName, dataRoot: dataRoot}
	d.apply(opts)
	return d, nil
}

func (d *KubeDriver) apply(opts []KubeOption) {
	for _, o := range opts {
		o(d)
	}
}

// CSIConfig configures the csi storage strategy.
type CSIConfig struct {
	// StorageClass provisions every pgoverlay PVC; it must support PVC
	// dataSource cloning (or VolumeSnapshots when SnapshotClass is set).
	StorageClass string
	// SnapshotClass switches branch cloning from PVC dataSource clones to
	// VolumeSnapshot + restore ("" = direct PVC clones).
	SnapshotClass string
	// VolumeSize is the storage request of every pgoverlay PVC ("" = 10Gi).
	VolumeSize string
}

// NewKubeDriverCSI connects to the cluster with the csi storage strategy:
// volumes are PVCs, branches are PVC clones, pods schedule on any node.
func NewKubeDriverCSI(kubeconfig, namespace string, csi CSIConfig, opts ...KubeOption) (*KubeDriver, error) {
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
	d.apply(opts)
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

// ListManagedVolumes returns every pgoverlay-managed volume name (delegated to
// the storage strategy: hostPath dirs or labelled PVCs).
func (d *KubeDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]VolumeInfo, error) {
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
// records the labels in <vol>/.pgoverlay-labels.json.
func (s *hostPathStorage) createVolume(ctx context.Context, name string, labels map[string]string) error {
	j, err := json.Marshal(withCreatedLabel(labels, time.Now()))
	if err != nil {
		return err
	}
	dir := dataRootMountPath + "/" + name
	cmd := fmt.Sprintf(`mkdir -p %s && printf '%%s' "$PGOVERLAY_VOLUME_LABELS" > %s/%s`, dir, dir, volumeLabelsFile)
	_, err = s.runRootHelper(ctx, cmd, []string{"PGOVERLAY_VOLUME_LABELS=" + string(j)})
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
	j, err := json.Marshal(withCreatedLabel(labels, time.Now()))
	if err != nil {
		return err
	}
	srcDir, dstDir := dataRootMountPath+"/"+src, dataRootMountPath+"/"+dst
	cmd := fmt.Sprintf(`rm -rf %s && mkdir -p %s && cp -a %s/. %s/ && printf '%%s' "$PGOVERLAY_VOLUME_LABELS" > %s/%s`,
		dstDir, dstDir, srcDir, dstDir, dstDir, volumeLabelsFile)
	_, err = s.runRootHelper(ctx, cmd, []string{"PGOVERLAY_VOLUME_LABELS=" + string(j)})
	return err
}

// listVolumes enumerates the volume dirs under the data root and returns only
// those whose .pgoverlay-labels.json records pgoverlay.instance=<instanceID>.
// Each managed volume dir is emitted on its own line followed by its label
// file's contents on the next, with listVolumesSentinel bracketing each entry;
// a dir whose marker is missing or names a different instance is foreign and
// skipped. A missing data root (nothing created yet) lists nothing.
func (s *hostPathStorage) listVolumes(ctx context.Context, instanceID string) ([]VolumeInfo, error) {
	out, err := s.runRootHelper(ctx, listVolumesScript(dataRootMountPath), nil)
	if err != nil {
		return nil, err
	}
	return parseVolumeList(out, instanceID), nil
}

// labelCreated records, in a hostPath volume's label file, when the volume was
// created (unix seconds). A plain directory has no reliable creation time of
// its own, and reconcile's volume GC skips volumes younger than its grace
// period. Volumes created before this label existed report no time.
const labelCreated = "pgoverlay.created"

// withCreatedLabel returns a copy of labels with labelCreated set to now.
func withCreatedLabel(labels map[string]string, now time.Time) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[labelCreated] = strconv.FormatInt(now.Unix(), 10)
	return out
}

// listVolumesScript prints, for every directory under root, the dir name on
// one line, then its label file's contents (possibly empty), then
// listVolumesSentinel. An empty or missing root prints nothing rather than
// failing. Split out from listVolumes so the script can be exercised against a
// real shell in the unit suite instead of only inside a live cluster.
func listVolumesScript(root string) string {
	return fmt.Sprintf(
		`for d in %s/*/; do [ -d "$d" ] || continue; n=$(basename "$d"); printf '%%s\n' "$n"; cat "$d/%s" 2>/dev/null; printf '\n%s\n'; done 2>/dev/null || true`,
		root, volumeLabelsFile, listVolumesSentinel)
}

// parseVolumeList picks out the volume dirs whose label file records
// pgoverlay.instance=<instanceID>. A dir whose marker is missing, unparseable,
// or names a different instance is foreign and is skipped, so reconcile never
// reclaims another instance's data.
func parseVolumeList(out, instanceID string) []VolumeInfo {
	var vols []VolumeInfo
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
			v := VolumeInfo{Name: name}
			if sec, err := strconv.ParseInt(labels[labelCreated], 10, 64); err == nil && sec > 0 {
				v.Created = time.Unix(sec, 0)
			}
			vols = append(vols, v)
		}
	}
	return vols
}

// listVolumesSentinel brackets each volume entry in the hostPath listVolumes
// helper output so a name can be split cleanly from its (possibly empty) label
// JSON.
//
// It is spliced into a shell script that is handed to the helper pod as an
// argv element ("sh", "-c", script), which constrains it in two ways:
//
//   - No NUL. execve(2) argv entries are NUL-terminated C strings, so a single
//     NUL anywhere in the script makes the exec fail with EINVAL before the
//     syscall is even attempted. runc reports that as "exec /bin/sh: invalid
//     argument" — which reads like a missing or incompatible shell and sends
//     you hunting the image, not the argument. This constant used to be
//     NUL-padded, and it broke every hostPath reconcile pass that way.
//   - No '%' and no single quote, because the sentinel lands inside a
//     single-quoted printf format string in that script.
//
// ASCII RS (0x1E) satisfies both and still cannot collide with either half of
// an entry: JSON requires control characters below 0x20 to be escaped, so a
// label file can never emit a literal one, and validVolumeName restricts names
// to an alphanumeric/dash set that excludes it.
const listVolumesSentinel = "\x1e--pgoverlay-vol--\x1e"

// runRootHelper runs sh -c cmd in a helper pod with the whole data root
// mounted at dataRootMountPath (needed to create/remove volume dirs).
func (s *hostPathStorage) runRootHelper(ctx context.Context, cmd string, env []string) (string, error) {
	spec := HelperSpec{Image: UtilityImage, Cmd: []string{"sh", "-c", cmd}, Env: env}
	return s.d.runHelper(ctx, spec, func(pod *corev1.Pod) {
		t := corev1.HostPathDirectoryOrCreate
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         "data-root",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: s.dataRoot, Type: &t}},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "data-root", MountPath: dataRootMountPath})
	})
}

// RunHelper runs spec to completion in a one-shot helper pod. The helper's
// environment travels in a short-lived Secret, never in the pod spec.
func (d *KubeDriver) RunHelper(ctx context.Context, spec HelperSpec) (string, error) {
	return d.runHelper(ctx, spec, nil)
}

const (
	// helperNamePrefix names helper pods and their env Secrets alike.
	helperNamePrefix = "pgoverlay-helper-"
	// defaultHelperStartTimeout bounds how long a helper pod may take to start
	// its container: scheduling, volume binding and attach, image pull. There
	// is no bound once it runs — a pg_basebackup of a large source can take
	// hours.
	defaultHelperStartTimeout = 10 * time.Minute
	// startFailureGrace is how long a helper may keep reporting one of
	// startFailureReasons before waitPodDone gives up on it. It rides out one
	// failed pull (a registry blip, a rate limit): the kubelet retries after
	// 10s, then 20s.
	startFailureGrace = 30 * time.Second
	// helperWatchTimeout ends each watch well before kube-apiserver's own
	// randomized 30-60 minute cut-off; waitPodDone reads the pod again and
	// re-watches either way.
	helperWatchTimeout = 5 * time.Minute
	// maxWatchFailures is how many Get/Watch failures in a row waitPodDone
	// tolerates (with backoff, about a minute and a half) before it reports
	// the API error.
	maxWatchFailures = 8
)

// errHelperGone reports a helper pod deleted before it reached a terminal
// phase: evicted, preempted, removed by an operator or garbage-collected.
var errHelperGone = errors.New("pod was deleted before it finished")

// runHelper creates the helper pod (and the Secret holding its environment),
// waits for a terminal phase (deadline from ctx), and always deletes both.
// customize, when set, amends the built pod: the hostPath root helper mounts
// the whole data root. The pod's captured logs are returned on success and
// embedded in the error on failure.
func (d *KubeDriver) runHelper(ctx context.Context, spec HelperSpec, customize func(*corev1.Pod)) (string, error) {
	if spec.Image == UtilityImage && d.helperImage != "" {
		spec.Image = d.helperImage
	}
	name, err := d.createHelper(ctx, spec, customize)
	if err != nil {
		return "", err
	}
	defer d.removeHelper(context.WithoutCancel(ctx), name)
	// The kubelet resolves env once, when it creates the container, and helper
	// pods never restart: the Secret is not needed past that point, so it goes
	// then rather than living for the whole (possibly hours-long) seed.
	started := func() { d.deleteHelperSecret(context.WithoutCancel(ctx), name) }
	phase, err := d.waitPodDone(ctx, name, started)
	if err != nil {
		return "", fmt.Errorf("helper pod %s: %w", name, err)
	}
	logs := d.podLogs(ctx, name)
	if phase != corev1.PodSucceeded {
		return logs, fmt.Errorf("helper pod %s failed: %s", name, logs)
	}
	return logs, nil
}

// createHelper creates the Secret holding the helper's environment (if it has
// any), then the pod reading it, under one fresh name, and returns that name.
// The name is generated here rather than with GenerateName so the pod can
// reference its Secret; a collision with an existing object just retries.
func (d *KubeDriver) createHelper(ctx context.Context, spec HelperSpec, customize func(*corev1.Pod)) (string, error) {
	for attempt := 1; ; attempt++ {
		meta := helperObjectMeta(d.namespace, helperNamePrefix+utilrand.String(5), d.instanceID, d.owner)
		pod := buildHelperPod(meta, d.storage, spec)
		if customize != nil {
			customize(pod)
		}
		retry := attempt < 3
		if secret := buildHelperSecret(meta, spec.Env); secret != nil {
			if _, err := d.cs.CoreV1().Secrets(d.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) && retry {
					continue
				}
				return "", fmt.Errorf("create helper env secret (branchd needs secrets create/delete in %s): %w", d.namespace, err)
			}
		}
		if _, err := d.cs.CoreV1().Pods(d.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			d.deleteHelperSecret(context.WithoutCancel(ctx), meta.Name)
			if apierrors.IsAlreadyExists(err) && retry {
				continue
			}
			return "", fmt.Errorf("create helper pod: %w", err)
		}
		return meta.Name, nil
	}
}

// removeHelper deletes a helper pod and its env Secret; either may be gone
// already.
func (d *KubeDriver) removeHelper(ctx context.Context, name string) {
	prop := metav1.DeletePropagationBackground
	err := d.cs.CoreV1().Pods(d.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &prop})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("kube: could not delete helper pod", "pod", name, "err", err)
	}
	d.deleteHelperSecret(ctx, name)
}

// deleteHelperSecret deletes a helper's env Secret. A missing one is fine: the
// helper had no environment, or the Secret was already deleted once its
// container started.
func (d *KubeDriver) deleteHelperSecret(ctx context.Context, name string) {
	err := d.cs.CoreV1().Secrets(d.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("kube: could not delete helper env secret", "secret", name, "err", err)
	}
}

// waitPodDone waits until the helper pod reaches a terminal phase and returns
// that phase. It survives the ends of its watches — kube-apiserver closes
// every watch after 30-60 minutes, and a long seed outlives several — by
// reading the pod again and re-watching. It fails when the pod is deleted,
// when its container keeps reporting a start failure for startFailureGrace,
// or when it has not started within the start timeout; each error carries
// what the pod's status said. started, when set, runs once, as soon as the
// pod's container has started.
func (d *KubeDriver) waitPodDone(ctx context.Context, name string, started func()) (corev1.PodPhase, error) {
	pods := d.cs.CoreV1().Pods(d.namespace)
	startTimeout := d.helperStartTimeout
	if startTimeout <= 0 {
		startTimeout = defaultHelperStartTimeout
	}
	p := &helperProgress{startTimeout: startTimeout, failureGrace: startFailureGrace, created: time.Now()}
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	watchTimeout := int64(helperWatchTimeout / time.Second)

	// observe records one sighting of the pod and reports whether waiting is
	// over (with the terminal phase, or an error).
	observe := func(pod *corev1.Pod) (corev1.PodPhase, bool, error) {
		phase, done, justStarted := p.observe(pod, time.Now())
		if justStarted && started != nil {
			started()
		}
		if done {
			return phase, true, nil
		}
		if err := p.check(time.Now()); err != nil {
			return "", true, err
		}
		return "", false, nil
	}
	// timeout fires when p next needs checking without a new sighting (nil
	// when there is nothing to time out on).
	timeout := func() <-chan time.Time {
		if dl := p.deadline(); !dl.IsZero() {
			return time.After(time.Until(dl))
		}
		return nil
	}
	// pause waits for d, unless the context ends or the wait times out first.
	pause := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return p.ctxErr(ctx.Err())
		case <-timeout():
			return p.check(time.Now())
		case <-time.After(d):
			return nil
		}
	}
	failures := 0
	// retry backs off after a failed API call; it gives up after
	// maxWatchFailures in a row, or when the wait itself times out. Errors
	// that retrying cannot fix (RBAC, a bad request) end the wait at once.
	retry := func(err error) error {
		failures++
		if failures >= maxWatchFailures || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
			apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
			return err
		}
		return pause(min(time.Duration(1<<(failures-1))*time.Second, 16*time.Second))
	}
	for {
		// Watch first, then read: a transition between the two shows up in
		// the watch rather than falling in a gap.
		watchStart := time.Now()
		w, err := pods.Watch(ctx, metav1.ListOptions{FieldSelector: selector, TimeoutSeconds: &watchTimeout})
		if err != nil {
			if ctx.Err() != nil {
				return "", p.ctxErr(ctx.Err())
			}
			if err := retry(fmt.Errorf("watch: %w", err)); err != nil {
				return "", err
			}
			continue
		}
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			w.Stop()
			return "", errHelperGone
		case err != nil:
			w.Stop()
			if ctx.Err() != nil {
				return "", p.ctxErr(ctx.Err())
			}
			if err := retry(fmt.Errorf("get: %w", err)); err != nil {
				return "", err
			}
			continue
		}
		failures = 0
		if phase, done, err := observe(pod); done {
			w.Stop()
			return phase, err
		}
		phase, done, err := d.followPodWatch(ctx, w, name, p, observe, timeout)
		w.Stop()
		if done {
			return phase, err
		}
		// A watch that ends right away (an error event, a proxy cutting
		// streams) must not turn this into a hot Get/Watch loop.
		if time.Since(watchStart) < time.Second {
			if err := pause(time.Second); err != nil {
				return "", err
			}
		}
	}
}

// followPodWatch consumes one watch on the helper pod until waiting is over
// (done) or the watch ends (not done: the caller reads the pod and watches
// again).
func (d *KubeDriver) followPodWatch(ctx context.Context, w watch.Interface, name string, p *helperProgress,
	observe func(*corev1.Pod) (corev1.PodPhase, bool, error), timeout func() <-chan time.Time) (corev1.PodPhase, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return "", true, p.ctxErr(ctx.Err())
		case <-timeout():
			if err := p.check(time.Now()); err != nil {
				return "", true, err
			}
		case ev, ok := <-w.ResultChan():
			if !ok {
				return "", false, nil // watch ended (timeout, apiserver cut-off): re-watch
			}
			switch ev.Type {
			case watch.Error:
				return "", false, nil // e.g. 410 Gone: read the pod again and re-watch
			case watch.Deleted:
				if pod, ok := ev.Object.(*corev1.Pod); ok && pod.Name == name {
					return "", true, errHelperGone
				}
				continue
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok || pod.Name != name {
				continue
			}
			if phase, done, err := observe(pod); done {
				return phase, true, err
			}
		}
	}
}

// helperProgress follows a helper pod towards a terminal phase and decides
// when waiting has become pointless. It holds no clock (callers pass the
// time), so the policy is unit-testable without an API server.
type helperProgress struct {
	startTimeout time.Duration // how long the container may take to start
	failureGrace time.Duration // how long a start failure may persist
	created      time.Time     // when waiting began

	started      bool
	failure      string    // start failure the pod reports now ("" = none)
	failureSince time.Time // when that failure was first seen
	last         *corev1.Pod
}

// observe records a sighting of pod at now. done reports a terminal phase;
// justStarted is true exactly once, on the first sighting with the container
// started.
func (p *helperProgress) observe(pod *corev1.Pod, now time.Time) (phase corev1.PodPhase, done, justStarted bool) {
	p.last = pod
	if !p.started && podStarted(pod) {
		p.started, justStarted = true, true
	}
	if f := podStartFailure(pod); f == "" {
		p.failure = ""
	} else if p.failure == "" {
		p.failure, p.failureSince = f, now
	} else {
		p.failure = f // same episode, possibly a new message (ErrImagePull -> ImagePullBackOff)
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return pod.Status.Phase, true, justStarted
	}
	return "", false, justStarted
}

// deadline is when check must run next if no new sighting arrives first (zero
// when nothing can time out: the container runs and reports no failure).
func (p *helperProgress) deadline() time.Time {
	var dl time.Time
	if !p.started {
		dl = p.created.Add(p.startTimeout)
	}
	if p.failure != "" {
		if f := p.failureSince.Add(p.failureGrace); dl.IsZero() || f.Before(dl) {
			dl = f
		}
	}
	return dl
}

// check returns an error once the pod has reported a start failure for
// failureGrace, or has not started its container within startTimeout.
func (p *helperProgress) check(now time.Time) error {
	if p.failure != "" && !now.Before(p.failureSince.Add(p.failureGrace)) {
		return fmt.Errorf("cannot start: %s", p.failure)
	}
	if !p.started && !now.Before(p.created.Add(p.startTimeout)) {
		return fmt.Errorf("not started after %s: %s", p.startTimeout, p.describe())
	}
	return nil
}

// ctxErr wraps a context error with the pod's state when it never started,
// which is usually the reason the caller ran out of time.
func (p *helperProgress) ctxErr(err error) error {
	if p.started || p.last == nil {
		return err
	}
	return fmt.Errorf("%w (pod not started: %s)", err, p.describe())
}

func (p *helperProgress) describe() string {
	if p.last == nil {
		return "no status seen"
	}
	return podNotRunning(p.last)
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
	// Exec into a container that is not running fails with a transport error
	// that says nothing about why. Callers poll readiness through exec (the
	// engine's pg_isready loop) and delete the pod once they give up, so the
	// pod's own account of the problem — unschedulable, ImagePullBackOff, a
	// missing Secret, CrashLoopBackOff and the last exit — has to travel in
	// this error or it is lost.
	if why := podNotRunning(pod); why != "" {
		return "", fmt.Errorf("pod %s is not running: %s", id, why)
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
		if apierrors.IsNotFound(err) {
			return ContainerInfo{}, fmt.Errorf("pod %s: %w: %w", id, ErrNotFound, err)
		}
		return ContainerInfo{}, err
	}
	return podInfo(pod), nil
}

// podInfo maps a pod onto ContainerInfo. Branch pods are bare Pods: an evicted
// or otherwise Failed pod stays Failed (restartPolicy only restarts
// containers), so Failed/Succeeded are Stopped. A pod being deleted is no
// longer serving but not yet gone, so it is neither Running nor Stopped. The
// pod IP is reported only while the pod runs: a Failed pod may keep
// status.podIP, but nothing listens there any more.
func podInfo(pod *corev1.Pod) ContainerInfo {
	info := ContainerInfo{
		ID:      pod.Name,
		Running: pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil,
		Stopped: pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded,
		Status:  string(pod.Status.Phase),
		Port:    5432,
		Created: pod.CreationTimestamp.Time,
		Labels:  pod.Labels,
	}
	if pod.Status.Reason != "" {
		info.Status += " (" + pod.Status.Reason + ")"
	}
	if pod.DeletionTimestamp != nil {
		info.Status += " (terminating)"
	}
	if info.Running {
		info.Host = pod.Status.PodIP
	}
	return info
}

// StopRemove deletes the pod (30s grace, background propagation) and waits
// until it is gone so a same-name recreate (branch reset) cannot collide.
// Removing a helper pod (an orphan a crashed branchd left behind) also
// removes its env Secret. Idempotent: NotFound is success.
func (d *KubeDriver) StopRemove(ctx context.Context, id string) error {
	if strings.HasPrefix(id, helperNamePrefix) {
		d.deleteHelperSecret(ctx, id)
	}
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
	return d.listPods(ctx, "pgoverlay.managed=true,pgoverlay.role=branch")
}

// ListHelpers lists the helper pods in the namespace, running or finished.
func (d *KubeDriver) ListHelpers(ctx context.Context) ([]ContainerInfo, error) {
	return d.listPods(ctx, "pgoverlay.managed=true,pgoverlay.role=helper")
}

func (d *KubeDriver) listPods(ctx context.Context, selector string) ([]ContainerInfo, error) {
	pods, err := d.cs.CoreV1().Pods(d.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, podInfo(&pods.Items[i]))
	}
	return out, nil
}
