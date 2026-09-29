package runtime

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pure pod-spec construction for the kube driver. Everything here is
// deterministic and unit-tested; kube.go owns the API calls.

const (
	helperContainerName = "helper"
	branchContainerName = "postgres"
	// dataRootMountPath is where volume-management helpers see the data root.
	dataRootMountPath = "/pgoverlay-root"
	// volumeLabelsFile records CreateVolume labels inside the volume dir
	// (no etcd objects for volumes — decision 3).
	volumeLabelsFile = ".pgoverlay-labels.json"
)

// volumeNameRe also guards against path traversal: volume names become
// subdirectories of the data root.
var volumeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,200}$`)

// boolPtr returns a *bool for the K8s tri-state fields (nil/true/false).
func boolPtr(b bool) *bool { return &b }

// noAutomount disables the default ServiceAccount-token mount. Branch pods run
// Postgres and helper pods do volume ops; neither touches the Kubernetes API,
// so an SA token in the pod would only be a credential to steal.
var noAutomount = boolPtr(false)

func validVolumeName(name string) error {
	if !volumeNameRe.MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
}

// volumeHostPath maps a logical volume name to its directory on the storage
// node (decision 1: volumes are subdirectories of the data root).
func volumeHostPath(dataRoot, volume string) string {
	return path.Join(dataRoot, volume)
}

func kubeEnv(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

// hostPathPodVolumes translates driver mounts to hostPath volumes + mounts.
// MountVolume maps to a dataRoot subdirectory (DirectoryOrCreate keeps
// CreateVolume trivial); MountHostPath maps the absolute path directly and
// requires it to exist (a zfs dataset mountpoint — a missing one is an error
// worth surfacing, not papering over with an empty dir).
func hostPathPodVolumes(dataRoot string, ms []Mount) ([]corev1.Volume, []corev1.VolumeMount) {
	vols := make([]corev1.Volume, 0, len(ms))
	mounts := make([]corev1.VolumeMount, 0, len(ms))
	for i, m := range ms {
		p, t := volumeHostPath(dataRoot, m.Volume), corev1.HostPathDirectoryOrCreate
		if m.Kind == MountHostPath {
			p, t = m.Volume, corev1.HostPathDirectory
		}
		name := fmt.Sprintf("vol-%d", i)
		vols = append(vols, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: p, Type: &t},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: m.Target, ReadOnly: m.ReadOnly})
	}
	return vols, mounts
}

// helperEnv renders a helper's environment as references into its env Secret
// (see buildHelperSecret) instead of literal values, so what a helper is
// handed — the source password for pg_basebackup/pg_dump above all — never
// appears in the Pod object: not in `kubectl get pod -o yaml`, not in etcd's
// copy of the pod, not in audit records of pod writes.
func helperEnv(secretName string, env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		out = append(out, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  k,
			},
		}})
	}
	return out
}

// buildHelperSecret renders the Secret that carries a helper's environment,
// or nil when the helper has none. It shares the helper pod's name, labels and
// owner, and is immutable. It only has to exist until the kubelet has started
// the container (environment is resolved once, at container creation, and
// helper pods never restart), so kube.go deletes it then, and again when the
// helper is removed.
func buildHelperSecret(meta metav1.ObjectMeta, env []string) *corev1.Secret {
	if len(env) == 0 {
		return nil
	}
	data := make(map[string][]byte, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		data[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: meta,
		Type:       corev1.SecretTypeOpaque,
		Immutable:  boolPtr(true),
		Data:       data,
	}
}

// helperObjectMeta is the metadata a helper pod and its env Secret share. The
// instance label (when known) lets orphan GC tell this registry's helpers from
// another instance's in the same namespace; the owner (branchd's own pod, when
// known) lets Kubernetes garbage-collect both once that pod is gone.
func helperObjectMeta(namespace, name, instanceID string, owner *metav1.OwnerReference) metav1.ObjectMeta {
	labels := map[string]string{"pgoverlay.managed": "true", "pgoverlay.role": "helper"}
	if instanceID != "" {
		labels[LabelInstance] = instanceID
	}
	meta := metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}
	if owner != nil {
		meta.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return meta
}

// helperSecurityContext is a helper container's security context. Every
// unprivileged helper runs under the container runtime's default seccomp
// profile with privilege escalation off: helpers copy files, chown and run
// pg_basebackup/pg_dump, none of which needs more, and an unset profile means
// Unconfined on any kubelet without seccompDefault. Privileged helpers (the
// zfs backend) get privileged mode instead, which implies an unconfined
// profile and cannot be combined with allowPrivilegeEscalation=false.
// SysAdmin helpers (the copy-up probe, which mounts an overlay) get exactly
// what a hostPath branch container has: SYS_ADMIN with seccomp and AppArmor
// unconfined.
//
// HelperSpec.User maps to a numeric runAs identity. Docker resolves names via
// the image's /etc/passwd; K8s cannot, so the one name pgoverlay uses
// ("postgres", uid/gid 999 in the official images) is mapped explicitly and
// numeric strings pass through. "" means the image default.
func helperSecurityContext(spec HelperSpec) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{}
	switch {
	case spec.Privileged:
		sc.Privileged = boolPtr(true)
	case spec.SysAdmin:
		// the hostPath branch container's posture (hostPathStorage.
		// branchSecurityContext), for helpers that mount an overlay
		unconfined := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		sc.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}
		sc.SeccompProfile = &unconfined
		sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	default:
		sc.AllowPrivilegeEscalation = boolPtr(false)
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	if spec.User != "" {
		uid := int64(999)
		if n, err := strconv.ParseInt(spec.User, 10, 64); err == nil {
			uid = n
		}
		sc.RunAsUser, sc.RunAsGroup = &uid, &uid
	}
	return sc
}

// buildHelperPod renders a one-shot helper pod (pinned to the storage node in
// hostPath mode; freely scheduled in csi mode). Its environment is read from
// the Secret of the same name (buildHelperSecret). HelperSpec.Network is
// ignored on K8s: the pod network reaches both cluster pods and external
// hosts, which is all helpers need. HelperSpec.HostDevices is also ignored: a
// privileged container sees host devices already.
func buildHelperPod(meta metav1.ObjectMeta, st kubeStorage, spec HelperSpec) *corev1.Pod {
	vols, mounts := st.podVolumes(spec.Mounts)
	return &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:                     st.nodeName(),
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: noAutomount,
			Volumes:                      vols,
			Containers: []corev1.Container{{
				Name:            helperContainerName,
				Image:           spec.Image,
				Command:         spec.Cmd,
				Env:             helperEnv(meta.Name, spec.Env),
				VolumeMounts:    mounts,
				SecurityContext: helperSecurityContext(spec),
			}},
		},
	}
}

// buildBranchPod renders a long-running branch pod (plain Pod, not a
// Deployment: branches are disposable and the engine reconciles). Placement
// and privileges come from the storage strategy: hostPath pins to the storage
// node and adds SYS_ADMIN for the in-container overlay mount; csi pods
// schedule anywhere with no extra capabilities, under RuntimeDefault seccomp.
func buildBranchPod(namespace string, st kubeStorage, spec BranchSpec) *corev1.Pod {
	vols, mounts := st.podVolumes(spec.Mounts)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    spec.Labels,
		},
		Spec: corev1.PodSpec{
			NodeName:                     st.nodeName(),
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: noAutomount,
			Volumes:                      vols,
			Containers: []corev1.Container{{
				Name:            branchContainerName,
				Image:           spec.Image,
				Command:         spec.Entrypoint,
				Env:             kubeEnv(spec.Env),
				VolumeMounts:    mounts,
				Ports:           []corev1.ContainerPort{{ContainerPort: 5432}},
				SecurityContext: st.branchSecurityContext(),
			}},
		},
	}
}

// Pod status diagnostics. A pod whose container cannot start (no node fits,
// an image that cannot be pulled, a missing Secret, a crashing entrypoint)
// shows why only in its status, and the pod is deleted as soon as the caller
// gives up, taking the evidence with it. These render that status into the
// error the caller sees.

// startFailureReasons are container waiting reasons the kubelet does not get
// past on its own: the image reference, the registry or the pod spec has to
// change first. A helper that keeps reporting one (see startFailureGrace) is
// failed instead of waiting out its start timeout.
var startFailureReasons = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true,
}

// podContainerStatus returns the status of the pod's first container, or nil
// when the kubelet has not reported one yet.
func podContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	if len(pod.Spec.Containers) == 0 {
		return nil
	}
	name := pod.Spec.Containers[0].Name
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// podStarted reports whether the pod's container has started (it runs, or it
// ran and exited).
func podStarted(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	cs := podContainerStatus(pod)
	return cs != nil && (cs.State.Running != nil || cs.State.Terminated != nil)
}

// podStartFailure describes a waiting reason from startFailureReasons the
// pod's container currently reports ("" when there is none).
func podStartFailure(pod *corev1.Pod) string {
	cs := podContainerStatus(pod)
	if cs == nil || cs.State.Waiting == nil || !startFailureReasons[cs.State.Waiting.Reason] {
		return ""
	}
	return fmt.Sprintf("container %s waiting: %s", cs.Name, reasonMessage(cs.State.Waiting.Reason, cs.State.Waiting.Message))
}

// podNotRunning explains why the pod's first container is not running —
// phase, scheduling, the container's waiting reason, its last exit — or
// returns "" when it runs (or when the kubelet has reported nothing that says
// otherwise yet).
func podNotRunning(pod *corev1.Pod) string {
	cs := podContainerStatus(pod)
	if pod.Status.Phase == corev1.PodRunning && (cs == nil || cs.State.Running != nil) {
		return ""
	}
	phase := pod.Status.Phase
	if phase == "" {
		phase = corev1.PodPending
	}
	parts := []string{"phase " + string(phase)}
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		parts = append(parts, reasonMessage(pod.Status.Reason, pod.Status.Message))
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			parts = append(parts, "not scheduled: "+reasonMessage(c.Reason, c.Message))
		}
	}
	if cs != nil {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			parts = append(parts, fmt.Sprintf("container %s waiting: %s", cs.Name, reasonMessage(w.Reason, w.Message)))
		}
		if t := cs.State.Terminated; t != nil {
			parts = append(parts, fmt.Sprintf("container %s exited %d: %s", cs.Name, t.ExitCode, reasonMessage(t.Reason, t.Message)))
		} else if t := cs.LastTerminationState.Terminated; t != nil {
			parts = append(parts, fmt.Sprintf("last exit %d: %s", t.ExitCode, reasonMessage(t.Reason, t.Message)))
		}
	}
	return strings.Join(parts, "; ")
}

// reasonMessage joins a status reason and its message, either of which may be
// empty.
func reasonMessage(reason, message string) string {
	message = strings.TrimSpace(message)
	switch {
	case reason == "":
		return message
	case message == "":
		return reason
	}
	return reason + ": " + message
}
