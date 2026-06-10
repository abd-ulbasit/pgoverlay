package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// KubeDriver runs branches as pods pinned to one storage node; "volumes" are
// subdirectories of dataRoot on that node, mounted via hostPath (decision 1:
// single-node dev/test scope; multi-node via CSI is future work).
// Container IDs are pod names.
type KubeDriver struct {
	cs        kubernetes.Interface
	cfg       *rest.Config // for exec (SPDY); nil only in unit tests
	namespace string
	nodeName  string
	dataRoot  string
}

const volumeHelperImage = "alpine:3.21"

// NewKubeDriver connects to the cluster. kubeconfig=="" uses in-cluster
// config when available, else the default kubeconfig loading rules
// (KUBECONFIG / ~/.kube/config).
func NewKubeDriver(kubeconfig, namespace, nodeName, dataRoot string) (*KubeDriver, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err == rest.ErrNotInCluster {
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
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
	return &KubeDriver{cs: cs, cfg: cfg, namespace: namespace, nodeName: nodeName, dataRoot: dataRoot}, nil
}

// EnsureImage is a no-op: the kubelet pulls images on pod start.
func (d *KubeDriver) EnsureImage(ctx context.Context, image string) error { return nil }

// CreateVolume mkdirs the volume dir on the storage node via a helper pod and
// records the labels in <vol>/.pgbranch-labels.json.
func (d *KubeDriver) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if err := validVolumeName(name); err != nil {
		return err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	dir := dataRootMountPath + "/" + name
	cmd := fmt.Sprintf(`mkdir -p %s && printf '%%s' "$PGBRANCH_VOLUME_LABELS" > %s/%s`, dir, dir, volumeLabelsFile)
	if _, err := d.runRootHelper(ctx, cmd, []string{"PGBRANCH_VOLUME_LABELS=" + string(j)}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// RemoveVolume deletes the volume dir on the storage node. Idempotent
// (rm -rf on a missing dir succeeds).
func (d *KubeDriver) RemoveVolume(ctx context.Context, name string) error {
	if err := validVolumeName(name); err != nil {
		return err
	}
	if _, err := d.runRootHelper(ctx, "rm -rf "+dataRootMountPath+"/"+name, nil); err != nil {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

// runRootHelper runs sh -c cmd in a helper pod with the whole data root
// mounted at dataRootMountPath (needed to create/remove volume dirs).
func (d *KubeDriver) runRootHelper(ctx context.Context, cmd string, env []string) (string, error) {
	pod := buildHelperPod(d.namespace, d.nodeName, d.dataRoot,
		HelperSpec{Image: volumeHelperImage, Cmd: []string{"sh", "-c", cmd}, Env: env})
	t := corev1.HostPathDirectoryOrCreate
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         "data-root",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: d.dataRoot, Type: &t}},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "data-root", MountPath: dataRootMountPath})
	return d.runPodToCompletion(ctx, pod)
}

func (d *KubeDriver) RunHelper(ctx context.Context, spec HelperSpec) (string, error) {
	return d.runPodToCompletion(ctx, buildHelperPod(d.namespace, d.nodeName, d.dataRoot, spec))
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
	pod := buildBranchPod(d.namespace, d.nodeName, d.dataRoot, spec)
	created, err := d.cs.CoreV1().Pods(d.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create branch pod: %w", err)
	}
	return created.Name, nil
}

// Exec runs cmd in the pod's first container and fails on non-zero exit with
// captured output, matching the docker driver's contract.
func (d *KubeDriver) Exec(ctx context.Context, id string, cmd []string) error {
	pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
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
		return err
	}
	var buf bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &buf, Stderr: &buf}); err != nil {
		return fmt.Errorf("exec %v: %w: %s", cmd, err, buf.String())
	}
	return nil
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
