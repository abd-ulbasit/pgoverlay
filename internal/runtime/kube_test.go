package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
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
		{"/var/lib/pgoverlay", "pgoverlay-src-main", "/var/lib/pgoverlay/pgoverlay-src-main"},
		{"/var/lib/pgoverlay/", "pgoverlay-br-pr-1-rw", "/var/lib/pgoverlay/pgoverlay-br-pr-1-rw"},
	}
	for _, c := range cases {
		if got := volumeHostPath(c.dataRoot, c.volume); got != c.want {
			t.Errorf("volumeHostPath(%q,%q) = %q, want %q", c.dataRoot, c.volume, got, c.want)
		}
	}
}

func TestValidVolumeName(t *testing.T) {
	for _, ok := range []string{"pgoverlay-src-main", "pgoverlay-br-pr-1-rw", "a"} {
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
			{Volume: "pgoverlay-src-main", Target: "/seed"},
		},
		Network: "ignored-on-k8s",
		User:    "postgres",
	}
	pod := buildHelperPod("pgb", &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgoverlay"}, spec)

	if pod.GenerateName != "pgoverlay-helper-" {
		t.Errorf("GenerateName = %q", pod.GenerateName)
	}
	if pod.Namespace != "pgb" {
		t.Errorf("Namespace = %q", pod.Namespace)
	}
	if pod.Labels["pgoverlay.managed"] != "true" || pod.Labels["pgoverlay.role"] != "helper" {
		t.Errorf("labels = %v", pod.Labels)
	}
	if pod.Spec.NodeName != "node-1" {
		t.Errorf("NodeName = %q", pod.Spec.NodeName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q", pod.Spec.RestartPolicy)
	}
	// Helper pods do volume ops, not Kubernetes API calls: no SA token mounted.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", pod.Spec.AutomountServiceAccountToken)
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
	if v.HostPath == nil || v.HostPath.Path != "/var/lib/pgoverlay/pgoverlay-src-main" {
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
	pod := buildHelperPod("default", &hostPathStorage{node: "n", dataRoot: "/var/lib/pgoverlay"}, HelperSpec{Image: "alpine:3.21", Cmd: []string{"true"}})
	if sc := pod.Spec.Containers[0].SecurityContext; sc != nil {
		t.Errorf("SecurityContext = %+v, want nil when User empty", sc)
	}
	if env := pod.Spec.Containers[0].Env; len(env) != 0 {
		t.Errorf("Env = %v, want empty", env)
	}
}

func TestBuildHelperPodPrivileged(t *testing.T) {
	// zfs helpers: privileged pod (a privileged container sees host devices,
	// so HostDevices needs no explicit kube mapping)
	pod := buildHelperPod("pgb", &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgoverlay"}, HelperSpec{
		Image:       "alpine:3.21",
		Cmd:         []string{"sh", "-c", "zfs snapshot tank/pgoverlay/src-main-g1@br-pr-1"},
		Privileged:  true,
		HostDevices: []string{"/dev/zfs"},
	})
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatalf("SecurityContext = %+v, want privileged", sc)
	}
	// privileged + user compose (not used today, but must not panic or drop one)
	pod = buildHelperPod("pgb", &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgoverlay"}, HelperSpec{
		Image: "alpine:3.21", Cmd: []string{"true"}, User: "postgres", Privileged: true,
	})
	sc = pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged || sc.RunAsUser == nil || *sc.RunAsUser != 999 {
		t.Fatalf("SecurityContext = %+v, want privileged + RunAsUser 999", sc)
	}
}

func TestBuildHelperPodHostPathMount(t *testing.T) {
	// MountHostPath mounts an absolute host path (a zfs dataset mountpoint)
	// directly — not a dataRoot subdirectory — and requires it to exist.
	pod := buildHelperPod("pgb", &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgoverlay"}, HelperSpec{
		Image: "alpine:3.21",
		Cmd:   []string{"true"},
		Mounts: []Mount{
			{Kind: MountHostPath, Volume: "/tank/pgoverlay/br-pr-1", Target: "/pgoverlay/rw"},
			{Volume: "pgoverlay-src-main", Target: "/seed"},
		},
	})
	v0 := pod.Spec.Volumes[0]
	if v0.HostPath == nil || v0.HostPath.Path != "/tank/pgoverlay/br-pr-1" {
		t.Fatalf("hostpath mount path = %+v, want /tank/pgoverlay/br-pr-1", v0.HostPath)
	}
	if v0.HostPath.Type == nil || *v0.HostPath.Type != corev1.HostPathDirectory {
		t.Errorf("hostpath mount type = %v, want Directory (must already exist)", v0.HostPath.Type)
	}
	v1 := pod.Spec.Volumes[1]
	if v1.HostPath == nil || v1.HostPath.Path != "/var/lib/pgoverlay/pgoverlay-src-main" {
		t.Fatalf("volume mount path = %+v, want dataRoot subdir", v1.HostPath)
	}
	if v1.HostPath.Type == nil || *v1.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("volume mount type = %v, want DirectoryOrCreate", v1.HostPath.Type)
	}
}

func TestBuildBranchPod(t *testing.T) {
	labels := map[string]string{
		"pgoverlay.managed": "true", "pgoverlay.role": "branch",
		"pgoverlay.branch.id": "b1", "pgoverlay.branch.name": "pr-1",
	}
	spec := BranchSpec{
		Name:  "pgoverlay-br-pr-1",
		Image: "postgres:17",
		Env:   []string{"PGDATA=/pgoverlay/merged", "PGOVERLAY_LOWERS=/pgoverlay/lower0/data"},
		Mounts: []Mount{
			{Volume: "pgoverlay-src-main", Target: "/pgoverlay/lower0", ReadOnly: true},
			{Volume: "pgoverlay-br-pr-1-rw", Target: "/pgoverlay/rw"},
		},
		Entrypoint: []string{"/bin/sh", "/pgoverlay/rw/entrypoint.sh"},
		Labels:     labels,
	}
	pod := buildBranchPod("pgb", &hostPathStorage{node: "node-1", dataRoot: "/var/lib/pgoverlay"}, spec)

	if pod.Name != "pgoverlay-br-pr-1" || pod.Namespace != "pgb" {
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
	// Branch pods run Postgres only: no Kubernetes API access, no SA token.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", pod.Spec.AutomountServiceAccountToken)
	}
	c := pod.Spec.Containers[0]
	if c.Name != "postgres" || c.Image != "postgres:17" {
		t.Errorf("container = %q/%q", c.Name, c.Image)
	}
	if len(c.Command) != 2 || c.Command[0] != "/bin/sh" || c.Command[1] != "/pgoverlay/rw/entrypoint.sh" {
		t.Errorf("Command = %v", c.Command)
	}
	if len(c.Env) != 2 || c.Env[0].Name != "PGDATA" || c.Env[0].Value != "/pgoverlay/merged" ||
		c.Env[1].Name != "PGOVERLAY_LOWERS" || c.Env[1].Value != "/pgoverlay/lower0/data" {
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
	if p := pod.Spec.Volumes[0].HostPath.Path; p != "/var/lib/pgoverlay/pgoverlay-src-main" {
		t.Errorf("volume[0] hostPath = %q", p)
	}
	if p := pod.Spec.Volumes[1].HostPath.Path; p != "/var/lib/pgoverlay/pgoverlay-br-pr-1-rw" {
		t.Errorf("volume[1] hostPath = %q", p)
	}
	if m := c.VolumeMounts[0]; m.MountPath != "/pgoverlay/lower0" || !m.ReadOnly {
		t.Errorf("mount[0] = %+v, want read-only /pgoverlay/lower0", m)
	}
	if m := c.VolumeMounts[1]; m.MountPath != "/pgoverlay/rw" || m.ReadOnly {
		t.Errorf("mount[1] = %+v, want rw /pgoverlay/rw", m)
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
	d := &KubeDriver{cs: cs, namespace: "default"}
	d.storage = &hostPathStorage{d: d, node: "n", dataRoot: "/var/lib/pgoverlay"}
	return d, cs
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
	out, err := d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"true"}})
	if err != nil {
		t.Fatalf("RunHelper = %v", err)
	}
	// the fake clientset serves "fake logs" for pod log requests; the real
	// driver captures the pod's output the same way on success.
	if !strings.Contains(out, "fake logs") {
		t.Errorf("RunHelper output %q does not include pod logs", out)
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
	_, err := d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"false"}})
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
	pod := buildBranchPod("default", &hostPathStorage{node: "n", dataRoot: "/var/lib/pgoverlay"}, BranchSpec{
		Name: "pgoverlay-br-x", Image: "postgres:17",
		Labels: map[string]string{"pgoverlay.managed": "true", "pgoverlay.role": "branch"},
	})
	if _, err := cs.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.244.0.7"
	if _, err := cs.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := d.Inspect(ctx, "pgoverlay-br-x")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "pgoverlay-br-x" || !info.Running || info.Host != "10.244.0.7" || info.Port != 5432 {
		t.Errorf("Inspect = %+v", info)
	}
	if info.Labels["pgoverlay.role"] != "branch" {
		t.Errorf("labels = %v", info.Labels)
	}
	list, err := d.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "pgoverlay-br-x" || !list[0].Running {
		t.Errorf("ListManaged = %+v", list)
	}
}

// TestHostPathHelperArgvIsExecSafe is the regression test for
// "exec /bin/sh: invalid argument", which took out every hostPath reconcile
// pass while looking like a broken helper image.
//
// The listVolumes helper script embedded a sentinel containing literal NUL
// bytes. execve(2) argv entries are NUL-terminated C strings, so the exec was
// rejected with EINVAL before it ever reached the shell; runc surfaced that as
// a message about /bin/sh, and reconcile aborted on step (d) every pass — no
// TTL reaping, no orphan GC, no layer GC — while the run counter kept
// incrementing as though it were converging.
//
// Nothing in the unit suite reached this path before: the helper-pod tests
// build specs against a fake clientset that never execs. So this asserts the
// property the kernel asserts, over every helper the hostPath storage builds.
func TestHostPathHelperArgvIsExecSafe(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	var mu sync.Mutex
	var built []*corev1.Pod
	cs.PrependReactor("create", "pods", func(action ktesting.Action) (bool, kruntime.Object, error) {
		mu.Lock()
		built = append(built, action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy())
		mu.Unlock()
		return false, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := d.storage.(*hostPathStorage)
	labels := map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-1"}

	// Every helper the hostPath driver can issue, including the one that broke.
	ops := []struct {
		name string
		run  func() error
	}{
		{"createVolume", func() error { return s.createVolume(ctx, "pgoverlay-src-main", labels) }},
		{"cloneVolume", func() error {
			return s.cloneVolume(ctx, "pgoverlay-src-main", "pgoverlay-br-pr-1-rw", labels)
		}},
		{"removeVolume", func() error { return s.removeVolume(ctx, "pgoverlay-br-pr-1-rw") }},
		{"listVolumes", func() error { _, err := s.listVolumes(ctx, "inst-1"); return err }},
	}
	for _, op := range ops {
		settlePods(cs, corev1.PodSucceeded)
		if err := op.run(); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(built) != len(ops) {
		t.Fatalf("built %d helper pods, want %d", len(built), len(ops))
	}
	for _, pod := range built {
		for _, c := range pod.Spec.Containers {
			args := append(append([]string{}, c.Command...), c.Args...)
			for _, e := range c.Env {
				args = append(args, e.Value)
			}
			for i, a := range args {
				// The exact conversion the exec path performs: it returns
				// EINVAL for any string containing a NUL byte.
				if _, err := syscall.BytePtrFromString(a); err != nil {
					t.Errorf("helper argv[%d] is not exec-safe: %v\nargv = %q", i, err, a)
				}
			}
		}
	}
}

// TestListVolumesSentinelIsShellSafe pins the other two constraints on the
// sentinel: it is spliced into a single-quoted printf format string in the
// helper script, so a '%' would be read as a conversion specifier and a single
// quote would end the quoting and reshape the command.
func TestListVolumesSentinelIsShellSafe(t *testing.T) {
	if strings.ContainsAny(listVolumesSentinel, "%'") {
		t.Errorf("listVolumesSentinel %q contains %% or ', which the shell printf format would reinterpret", listVolumesSentinel)
	}
	if strings.Contains(listVolumesSentinel, "\x00") {
		t.Errorf("listVolumesSentinel %q contains NUL: execve would reject the helper argv with EINVAL", listVolumesSentinel)
	}
	if listVolumesSentinel == "" {
		t.Error("listVolumesSentinel is empty: listVolumes output could not be split into entries")
	}
}

// TestListVolumesRoundTripsThroughRealShell exercises the hostPath volume
// listing end to end without a cluster: the real script, a real /bin/sh, and
// the real parser. exec.Command performs the same argv conversion the kubelet's
// runtime does, so a sentinel that cannot be exec'd fails here exactly as it
// failed in kind — which is the coverage gap that let the NUL sentinel ship
// (kube_test.go only ever built pod specs, and only the live kind IT ran them).
func TestListVolumesRoundTripsThroughRealShell(t *testing.T) {
	root := t.TempDir()
	write := func(dir string, labels map[string]string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if labels == nil {
			return // a dir with no marker at all
		}
		j, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, volumeLabelsFile), j, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pgoverlay-src-main", map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-1"})
	write("pgoverlay-br-pr-1-rw", map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-1"})
	write("pgoverlay-src-other", map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst-2"})
	write("someone-elses-data", nil)

	out, err := exec.Command("/bin/sh", "-c", listVolumesScript(root)).Output()
	if err != nil {
		t.Fatalf("running the helper script through /bin/sh: %v", err)
	}
	got := parseVolumeList(string(out), "inst-1")
	sort.Strings(got)
	want := []string{"pgoverlay-br-pr-1-rw", "pgoverlay-src-main"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listVolumes = %v, want %v (foreign and unlabelled dirs must be skipped)", got, want)
	}
}

// TestListVolumesEmptyRoot: a data root with nothing in it (or no data root at
// all) lists nothing and is not an error — reconcile runs before any volume
// exists.
func TestListVolumesEmptyRoot(t *testing.T) {
	for _, root := range []string{t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist")} {
		out, err := exec.Command("/bin/sh", "-c", listVolumesScript(root)).Output()
		if err != nil {
			t.Fatalf("root %q: %v", root, err)
		}
		if got := parseVolumeList(string(out), "inst-1"); len(got) != 0 {
			t.Errorf("root %q: listVolumes = %v, want none", root, got)
		}
	}
}
