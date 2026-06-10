package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestVolumeHostPath(t *testing.T) {
	cases := []struct{ dataRoot, volume, want string }{
		{"/var/lib/pgbranch", "pgbranch-src-main", "/var/lib/pgbranch/pgbranch-src-main"},
		{"/var/lib/pgbranch/", "pgbranch-br-pr-1-rw", "/var/lib/pgbranch/pgbranch-br-pr-1-rw"},
	}
	for _, c := range cases {
		if got := volumeHostPath(c.dataRoot, c.volume); got != c.want {
			t.Errorf("volumeHostPath(%q,%q) = %q, want %q", c.dataRoot, c.volume, got, c.want)
		}
	}
}

func TestValidVolumeName(t *testing.T) {
	for _, ok := range []string{"pgbranch-src-main", "pgbranch-br-pr-1-rw", "a"} {
		if err := validVolumeName(ok); err != nil {
			t.Errorf("validVolumeName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "a b", "a/b", "name$", ".hidden"} {
		if err := validVolumeName(bad); err == nil {
			t.Errorf("validVolumeName(%q) = nil, want error", bad)
		}
	}
}

func TestBuildHelperPod(t *testing.T) {
	spec := HelperSpec{
		Image: "postgres:17",
		Cmd:   []string{"pg_basebackup", "-D", "/seed/data"},
		Env:   []string{"PGPASSWORD=secret"},
		Mounts: []Mount{
			{Volume: "pgbranch-src-main", Target: "/seed"},
		},
		Network: "ignored-on-k8s",
		User:    "postgres",
	}
	pod := buildHelperPod("pgb", "node-1", "/var/lib/pgbranch", spec)

	if pod.GenerateName != "pgbranch-helper-" {
		t.Errorf("GenerateName = %q", pod.GenerateName)
	}
	if pod.Namespace != "pgb" {
		t.Errorf("Namespace = %q", pod.Namespace)
	}
	if pod.Labels["pgbranch.managed"] != "true" || pod.Labels["pgbranch.role"] != "helper" {
		t.Errorf("labels = %v", pod.Labels)
	}
	if pod.Spec.NodeName != "node-1" {
		t.Errorf("NodeName = %q", pod.Spec.NodeName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Name != "helper" || c.Image != "postgres:17" {
		t.Errorf("container = %q/%q", c.Name, c.Image)
	}
	if len(c.Command) != 3 || c.Command[0] != "pg_basebackup" {
		t.Errorf("Command = %v", c.Command)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "PGPASSWORD" || c.Env[0].Value != "secret" {
		t.Errorf("Env = %v", c.Env)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 999 {
		t.Errorf("SecurityContext = %+v, want RunAsUser 999 for user postgres", c.SecurityContext)
	}
	if c.SecurityContext.RunAsGroup == nil || *c.SecurityContext.RunAsGroup != 999 {
		t.Errorf("RunAsGroup = %v, want 999", c.SecurityContext.RunAsGroup)
	}
	if len(pod.Spec.Volumes) != 1 || len(c.VolumeMounts) != 1 {
		t.Fatalf("volumes/mounts = %d/%d", len(pod.Spec.Volumes), len(c.VolumeMounts))
	}
	v := pod.Spec.Volumes[0]
	if v.HostPath == nil || v.HostPath.Path != "/var/lib/pgbranch/pgbranch-src-main" {
		t.Errorf("hostPath = %+v", v.HostPath)
	}
	if v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("hostPath type = %v", v.HostPath.Type)
	}
	m := c.VolumeMounts[0]
	if m.Name != v.Name || m.MountPath != "/seed" || m.ReadOnly {
		t.Errorf("volumeMount = %+v", m)
	}
}

func TestBuildHelperPodNoUser(t *testing.T) {
	pod := buildHelperPod("default", "n", "/var/lib/pgbranch", HelperSpec{Image: "alpine:3.21", Cmd: []string{"true"}})
	if sc := pod.Spec.Containers[0].SecurityContext; sc != nil {
		t.Errorf("SecurityContext = %+v, want nil when User empty", sc)
	}
	if env := pod.Spec.Containers[0].Env; len(env) != 0 {
		t.Errorf("Env = %v, want empty", env)
	}
}

func TestBuildBranchPod(t *testing.T) {
	labels := map[string]string{
		"pgbranch.managed": "true", "pgbranch.role": "branch",
		"pgbranch.branch.id": "b1", "pgbranch.branch.name": "pr-1",
	}
	spec := BranchSpec{
		Name:  "pgbranch-br-pr-1",
		Image: "postgres:17",
		Env:   []string{"PGDATA=/pgbranch/merged", "PGBRANCH_LOWERS=/pgbranch/lower0/data"},
		Mounts: []Mount{
			{Volume: "pgbranch-src-main", Target: "/pgbranch/lower0", ReadOnly: true},
			{Volume: "pgbranch-br-pr-1-rw", Target: "/pgbranch/rw"},
		},
		Entrypoint: []string{"/bin/sh", "/pgbranch/rw/entrypoint.sh"},
		Labels:     labels,
	}
	pod := buildBranchPod("pgb", "node-1", "/var/lib/pgbranch", spec)

	if pod.Name != "pgbranch-br-pr-1" || pod.Namespace != "pgb" {
		t.Errorf("name/ns = %q/%q", pod.Name, pod.Namespace)
	}
	for k, v := range labels {
		if pod.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if pod.Spec.NodeName != "node-1" {
		t.Errorf("NodeName = %q", pod.Spec.NodeName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q", pod.Spec.RestartPolicy)
	}
	c := pod.Spec.Containers[0]
	if c.Name != "postgres" || c.Image != "postgres:17" {
		t.Errorf("container = %q/%q", c.Name, c.Image)
	}
	if len(c.Command) != 2 || c.Command[0] != "/bin/sh" || c.Command[1] != "/pgbranch/rw/entrypoint.sh" {
		t.Errorf("Command = %v", c.Command)
	}
	if len(c.Env) != 2 || c.Env[0].Name != "PGDATA" || c.Env[0].Value != "/pgbranch/merged" ||
		c.Env[1].Name != "PGBRANCH_LOWERS" || c.Env[1].Value != "/pgbranch/lower0/data" {
		t.Errorf("Env = %v", c.Env)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 5432 {
		t.Errorf("Ports = %v", c.Ports)
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Add) != 1 || c.SecurityContext.Capabilities.Add[0] != "SYS_ADMIN" {
		t.Errorf("SecurityContext = %+v, want capability SYS_ADMIN", c.SecurityContext)
	}
	if len(pod.Spec.Volumes) != 2 || len(c.VolumeMounts) != 2 {
		t.Fatalf("volumes/mounts = %d/%d", len(pod.Spec.Volumes), len(c.VolumeMounts))
	}
	if p := pod.Spec.Volumes[0].HostPath.Path; p != "/var/lib/pgbranch/pgbranch-src-main" {
		t.Errorf("volume[0] hostPath = %q", p)
	}
	if p := pod.Spec.Volumes[1].HostPath.Path; p != "/var/lib/pgbranch/pgbranch-br-pr-1-rw" {
		t.Errorf("volume[1] hostPath = %q", p)
	}
	if m := c.VolumeMounts[0]; m.MountPath != "/pgbranch/lower0" || !m.ReadOnly {
		t.Errorf("mount[0] = %+v, want read-only /pgbranch/lower0", m)
	}
	if m := c.VolumeMounts[1]; m.MountPath != "/pgbranch/rw" || m.ReadOnly {
		t.Errorf("mount[1] = %+v, want rw /pgbranch/rw", m)
	}
}

// fakeKubeDriver wires a KubeDriver to a fake clientset, with name generation
// for generateName pods (the fake API server may not fill it).
func fakeKubeDriver(t *testing.T) (*KubeDriver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	n := 0
	cs.PrependReactor("create", "pods", func(action ktesting.Action) (bool, kruntime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Name == "" && pod.GenerateName != "" {
			n++
			pod.Name = pod.GenerateName + "x" + string(rune('0'+n))
		}
		return false, nil, nil
	})
	return &KubeDriver{cs: cs, namespace: "default", nodeName: "n", dataRoot: "/var/lib/pgbranch"}, cs
}

// settlePods flips every pod the fake API server sees to the given phase, so
// RunHelper's watch loop completes.
func settlePods(cs *fake.Clientset, phase corev1.PodPhase) {
	go func() {
		for i := 0; i < 500; i++ {
			pods, _ := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
			for _, p := range pods.Items {
				if p.Status.Phase != phase {
					q := p.DeepCopy()
					q.Status.Phase = phase
					cs.CoreV1().Pods("default").UpdateStatus(context.Background(), q, metav1.UpdateOptions{})
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestRunHelperSuccessDeletesPod(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodSucceeded)
	if err := d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"true"}}); err != nil {
		t.Fatalf("RunHelper = %v", err)
	}
	pods, _ := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("helper pod not deleted: %d left", len(pods.Items))
	}
}

func TestRunHelperFailureIncludesLogsAndDeletesPod(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodFailed)
	err := d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"false"}})
	if err == nil {
		t.Fatal("want error from failed helper")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error %q does not mention failure", err)
	}
	// the fake clientset serves "fake logs" for pod log requests; the real
	// driver attaches the pod's last log lines the same way.
	if !strings.Contains(err.Error(), "fake logs") {
		t.Errorf("error %q does not include pod logs", err)
	}
	pods, _ := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("failed helper pod not deleted: %d left", len(pods.Items))
	}
}

func TestStopRemoveIdempotent(t *testing.T) {
	d, _ := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.StopRemove(ctx, "no-such-pod"); err != nil {
		t.Errorf("StopRemove on missing pod = %v, want nil", err)
	}
}

func TestKubeInspectAndListManaged(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pod := buildBranchPod("default", "n", "/var/lib/pgbranch", BranchSpec{
		Name: "pgbranch-br-x", Image: "postgres:17",
		Labels: map[string]string{"pgbranch.managed": "true", "pgbranch.role": "branch"},
	})
	if _, err := cs.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.244.0.7"
	if _, err := cs.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := d.Inspect(ctx, "pgbranch-br-x")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "pgbranch-br-x" || !info.Running || info.Host != "10.244.0.7" || info.Port != 5432 {
		t.Errorf("Inspect = %+v", info)
	}
	if info.Labels["pgbranch.role"] != "branch" {
		t.Errorf("labels = %v", info.Labels)
	}
	list, err := d.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "pgbranch-br-x" || !list[0].Running {
		t.Errorf("ListManaged = %+v", list)
	}
}
