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
	dataRootMountPath = "/pgbranch-root"
	// volumeLabelsFile records CreateVolume labels inside the volume dir
	// (no etcd objects for volumes — decision 3).
	volumeLabelsFile = ".pgbranch-labels.json"
)

// volumeNameRe also guards against path traversal: volume names become
// subdirectories of the data root.
var volumeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,200}$`)

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

// kubePodVolumes translates driver mounts to hostPath volumes + mounts.
// MountVolume maps to a dataRoot subdirectory (DirectoryOrCreate keeps
// CreateVolume trivial); MountHostPath maps the absolute path directly and
// requires it to exist (a zfs dataset mountpoint — a missing one is an error
// worth surfacing, not papering over with an empty dir).
func kubePodVolumes(dataRoot string, ms []Mount) ([]corev1.Volume, []corev1.VolumeMount) {
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

// helperSecurityContext maps HelperSpec.User to a numeric runAs identity.
// Docker resolves names via the image's /etc/passwd; K8s cannot, so the one
// name pgbranch uses ("postgres", uid/gid 999 in the official images) is
// mapped explicitly and numeric strings pass through. "" means image default.
func helperSecurityContext(user string) *corev1.SecurityContext {
	if user == "" {
		return nil
	}
	uid := int64(999)
	if n, err := strconv.ParseInt(user, 10, 64); err == nil {
		uid = n
	}
	return &corev1.SecurityContext{RunAsUser: &uid, RunAsGroup: &uid}
}

// buildHelperPod renders a one-shot helper pod pinned to the storage node.
// HelperSpec.Network is ignored on K8s: the pod network reaches both cluster
// pods and external hosts, which is all helpers need. HelperSpec.HostDevices
// is also ignored: a privileged container sees host devices already.
func buildHelperPod(namespace, nodeName, dataRoot string, spec HelperSpec) *corev1.Pod {
	vols, mounts := kubePodVolumes(dataRoot, spec.Mounts)
	sc := helperSecurityContext(spec.User)
	if spec.Privileged {
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		priv := true
		sc.Privileged = &priv
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pgbranch-helper-",
			Namespace:    namespace,
			Labels:       map[string]string{"pgbranch.managed": "true", "pgbranch.role": "helper"},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes:       vols,
			Containers: []corev1.Container{{
				Name:            helperContainerName,
				Image:           spec.Image,
				Command:         spec.Cmd,
				Env:             kubeEnv(spec.Env),
				VolumeMounts:    mounts,
				SecurityContext: sc,
			}},
		},
	}
}

// buildBranchPod renders a long-running branch pod (plain Pod, not a
// Deployment: branches are disposable and the engine reconciles). SYS_ADMIN
// is required for the in-container overlay mount, same as the docker driver.
func buildBranchPod(namespace, nodeName, dataRoot string, spec BranchSpec) *corev1.Pod {
	vols, mounts := kubePodVolumes(dataRoot, spec.Mounts)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    spec.Labels,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes:       vols,
			Containers: []corev1.Container{{
				Name:         branchContainerName,
				Image:        spec.Image,
				Command:      spec.Entrypoint,
				Env:          kubeEnv(spec.Env),
				VolumeMounts: mounts,
				Ports:        []corev1.ContainerPort{{ContainerPort: 5432}},
				SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
				},
			}},
		},
	}
}
