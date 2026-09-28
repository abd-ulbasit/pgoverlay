package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// createdObjects returns the objects of every recorded create of resource.
func createdObjects(cs *fake.Clientset, resource string) []kruntime.Object {
	var out []kruntime.Object
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == resource {
			out = append(out, a.(ktesting.CreateAction).GetObject())
		}
	}
	return out
}

// The source password reaches the seed helper through a Secret that exists
// only while it is needed: never as a literal in the Pod object.
func TestRunHelperPassesEnvThroughSecret(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	d.instanceID = "inst-1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodSucceeded)
	if _, err := d.RunHelper(ctx, HelperSpec{
		Image: "postgres:17",
		Cmd:   []string{"pg_basebackup"},
		Env:   []string{"PGPASSWORD=s3cret-prod-pw", "PGUSER=replicator"},
	}); err != nil {
		t.Fatalf("RunHelper = %v", err)
	}

	secrets := createdObjects(cs, "secrets")
	pods := createdObjects(cs, "pods")
	if len(secrets) != 1 || len(pods) != 1 {
		t.Fatalf("created %d secrets / %d pods, want 1 / 1", len(secrets), len(pods))
	}
	secret, pod := secrets[0].(*corev1.Secret), pods[0].(*corev1.Pod)
	if secret.Name != pod.Name || !strings.HasPrefix(pod.Name, helperNamePrefix) {
		t.Errorf("secret %q / pod %q: want one shared %s* name", secret.Name, pod.Name, helperNamePrefix)
	}
	if string(secret.Data["PGPASSWORD"]) != "s3cret-prod-pw" || string(secret.Data["PGUSER"]) != "replicator" {
		t.Errorf("secret data = %v", secret.Data)
	}
	if secret.Labels[LabelInstance] != "inst-1" || pod.Labels[LabelInstance] != "inst-1" {
		t.Errorf("instance labels: secret %v, pod %v", secret.Labels, pod.Labels)
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret-prod-pw") {
		t.Fatalf("the pod object carries the password:\n%s", raw)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != secret.Name {
			t.Errorf("env %s = %+v, want a secretKeyRef into %s", e.Name, e, secret.Name)
		}
	}
	// the Secret is created before the pod that reads it
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			order = append(order, a.GetResource().Resource)
		}
	}
	if strings.Join(order, ",") != "secrets,pods" {
		t.Errorf("create order = %v, want secrets before pods", order)
	}
	// and both are gone afterwards
	if l, _ := cs.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("%d helper secrets left behind", len(l.Items))
	}
	if l, _ := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("%d helper pods left behind", len(l.Items))
	}
}

// A helper without environment needs no Secret at all.
func TestRunHelperWithoutEnvCreatesNoSecret(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodSucceeded)
	if _, err := d.RunHelper(ctx, HelperSpec{Image: UtilityImage, Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(createdObjects(cs, "secrets")); n != 0 {
		t.Errorf("created %d secrets for a helper without environment", n)
	}
}

// A failed pod create must not leave the password Secret behind.
func TestRunHelperPodCreateFailureRemovesSecret(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	cs.PrependReactor("create", "pods", func(ktesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("admission webhook denied the pod")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.RunHelper(ctx, HelperSpec{Image: "postgres:17", Cmd: []string{"true"}, Env: []string{"PGPASSWORD=x"}})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("RunHelper = %v, want the pod create error", err)
	}
	if l, _ := cs.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("%d helper secrets left behind after a failed pod create", len(l.Items))
	}
}

// --kube-helper-image replaces UtilityImage (including in the hostPath root
// helper) and nothing else.
func TestHelperImageOverride(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	WithHelperImage("registry.internal/mirror/alpine@sha256:abc")(d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, image := range []string{UtilityImage, "postgres:17"} {
		settlePods(cs, corev1.PodSucceeded)
		if _, err := d.RunHelper(ctx, HelperSpec{Image: image, Cmd: []string{"true"}}); err != nil {
			t.Fatal(err)
		}
	}
	settlePods(cs, corev1.PodSucceeded)
	if err := d.RemoveVolume(ctx, "pgoverlay-br-pr-1-rw"); err != nil { // root helper
		t.Fatal(err)
	}
	var images []string
	for _, o := range createdObjects(cs, "pods") {
		images = append(images, o.(*corev1.Pod).Spec.Containers[0].Image)
	}
	want := []string{"registry.internal/mirror/alpine@sha256:abc", "postgres:17", "registry.internal/mirror/alpine@sha256:abc"}
	if strings.Join(images, " ") != strings.Join(want, " ") {
		t.Errorf("helper images = %v, want %v", images, want)
	}

	// without an override the root helper runs the digest-pinned UtilityImage
	d, cs = fakeKubeDriver(t)
	settlePods(cs, corev1.PodSucceeded)
	if err := d.RemoveVolume(ctx, "pgoverlay-br-pr-1-rw"); err != nil {
		t.Fatal(err)
	}
	if got := createdObjects(cs, "pods")[0].(*corev1.Pod).Spec.Containers[0].Image; got != UtilityImage {
		t.Errorf("root helper image = %q, want %q", got, UtilityImage)
	}
}

// Helpers are owned by branchd's pod (so kube GC removes them if branchd dies
// mid-seed), but only when that pod lives in the driver's namespace.
func TestWithOwnerPod(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	WithOwnerPod("other-ns", "branchd-abc", "uid-9")(d)
	if d.owner != nil {
		t.Fatalf("owner in another namespace accepted: %+v", d.owner)
	}
	WithOwnerPod("default", "branchd-abc", "")(d)
	if d.owner != nil {
		t.Fatalf("owner without a uid accepted: %+v", d.owner)
	}
	WithOwnerPod("default", "branchd-abc", "uid-9")(d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlePods(cs, corev1.PodSucceeded)
	if _, err := d.RunHelper(ctx, HelperSpec{Image: UtilityImage, Cmd: []string{"true"}, Env: []string{"A=b"}}); err != nil {
		t.Fatal(err)
	}
	for _, o := range append(createdObjects(cs, "pods"), createdObjects(cs, "secrets")...) {
		m := o.(metav1.Object)
		refs := m.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "Pod" || refs[0].Name != "branchd-abc" || refs[0].UID != "uid-9" {
			t.Errorf("%s owner references = %+v, want branchd's pod", m.GetName(), refs)
		}
		// no blockOwnerDeletion: that would need update on pods/finalizers
		if len(refs) == 1 && refs[0].BlockOwnerDeletion != nil {
			t.Errorf("%s: blockOwnerDeletion set", m.GetName())
		}
	}
}

// createPendingPod creates a helper-like pod in phase Pending.
func createPendingPod(t *testing.T, cs *fake.Clientset, name string, status corev1.PodStatus) {
	t.Helper()
	if status.Phase == "" {
		status.Phase = corev1.PodPending
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: helperContainerName}}},
		Status:     status,
	}
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func setPhase(t *testing.T, cs *fake.Clientset, name string, phase corev1.PodPhase) {
	t.Helper()
	pod, err := cs.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Error(err)
		return
	}
	pod.Status.Phase = phase
	if _, err := cs.CoreV1().Pods("default").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Error(err)
	}
}

// KUBE-02: kube-apiserver ends every watch after 30-60 minutes. That used to
// fail the helper ("watch closed before pod finished") and delete it mid-seed;
// waitPodDone must read the pod again and re-watch instead.
func TestWaitPodDoneSurvivesWatchClosure(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	createPendingPod(t, cs, "pgoverlay-helper-w", corev1.PodStatus{Phase: corev1.PodRunning})
	var watches atomic.Int32
	rewatched := make(chan struct{})
	cs.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
		switch watches.Add(1) {
		case 1: // the apiserver cuts the first watch off
			fw := watch.NewFake()
			fw.Stop()
			return true, fw, nil
		case 2:
			close(rewatched)
		}
		return false, nil, nil // the fake clientset's own watch
	})
	go func() {
		<-rewatched
		setPhase(t, cs, "pgoverlay-helper-w", corev1.PodSucceeded)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	phase, err := d.waitPodDone(ctx, "pgoverlay-helper-w", nil)
	if err != nil {
		t.Fatalf("waitPodDone = %v, want it to re-watch after the watch closed", err)
	}
	if phase != corev1.PodSucceeded {
		t.Errorf("phase = %q, want Succeeded", phase)
	}
	if n := watches.Load(); n < 2 {
		t.Errorf("watches = %d, want a re-watch", n)
	}
}

// An error event (e.g. 410 Gone for an expired resourceVersion) also just
// means: read the pod again and re-watch.
func TestWaitPodDoneRewatchesAfterErrorEvent(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	createPendingPod(t, cs, "pgoverlay-helper-e", corev1.PodStatus{Phase: corev1.PodRunning})
	var watches atomic.Int32
	rewatched := make(chan struct{})
	cs.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
		switch watches.Add(1) {
		case 1:
			fw := watch.NewFakeWithChanSize(1, false)
			fw.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired})
			return true, fw, nil
		case 2:
			close(rewatched)
		}
		return false, nil, nil
	})
	go func() {
		<-rewatched
		setPhase(t, cs, "pgoverlay-helper-e", corev1.PodFailed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	phase, err := d.waitPodDone(ctx, "pgoverlay-helper-e", nil)
	if err != nil || phase != corev1.PodFailed {
		t.Fatalf("waitPodDone = %q, %v; want Failed after re-watching", phase, err)
	}
}

// A helper deleted from under us (evicted, preempted, kubectl delete, GC) is
// a failure reported at once, not a hang.
func TestWaitPodDoneDeletedPod(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	createPendingPod(t, cs, "pgoverlay-helper-d", corev1.PodStatus{Phase: corev1.PodRunning})
	watching := make(chan struct{})
	var once atomic.Bool
	cs.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
		if once.CompareAndSwap(false, true) {
			defer close(watching)
		}
		return false, nil, nil
	})
	go func() {
		<-watching
		time.Sleep(50 * time.Millisecond) // let waitPodDone read the pod and follow the watch
		cs.CoreV1().Pods("default").Delete(context.Background(), "pgoverlay-helper-d", metav1.DeleteOptions{})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.waitPodDone(ctx, "pgoverlay-helper-d", nil); !errors.Is(err, errHelperGone) {
		t.Fatalf("waitPodDone = %v, want errHelperGone", err)
	}
	// and a pod that is already gone fails the same way
	if _, err := d.waitPodDone(ctx, "pgoverlay-helper-never", nil); !errors.Is(err, errHelperGone) {
		t.Fatalf("waitPodDone(missing) = %v, want errHelperGone", err)
	}
}

// A helper that never gets a node is failed after the start timeout, with the
// scheduler's reason, instead of hanging the request forever.
func TestWaitPodDoneStartTimeoutCarriesReason(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	d.helperStartTimeout = 200 * time.Millisecond
	createPendingPod(t, cs, "pgoverlay-helper-p", corev1.PodStatus{
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/3 nodes are available: 3 node(s) had volume node affinity conflict.",
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := d.waitPodDone(ctx, "pgoverlay-helper-p", nil)
	if err == nil {
		t.Fatal("waitPodDone returned nil for a pod that never started")
	}
	for _, want := range []string{"not started after", "Unschedulable", "volume node affinity conflict"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// started runs exactly once, when the container starts; RunHelper uses it to
// delete the env Secret early.
func TestWaitPodDoneStartedHook(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	createPendingPod(t, cs, "pgoverlay-helper-s", corev1.PodStatus{})
	var calls atomic.Int32
	watching := make(chan struct{})
	var once atomic.Bool
	cs.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
		if once.CompareAndSwap(false, true) {
			defer close(watching)
		}
		return false, nil, nil
	})
	go func() {
		<-watching
		time.Sleep(50 * time.Millisecond)
		setPhase(t, cs, "pgoverlay-helper-s", corev1.PodRunning)
		time.Sleep(50 * time.Millisecond)
		setPhase(t, cs, "pgoverlay-helper-s", corev1.PodSucceeded)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.waitPodDone(ctx, "pgoverlay-helper-s", func() { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("started hook ran %d times, want 1", n)
	}
}

func imagePullBackOff(msg string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  helperContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: msg}},
		}},
	}
}

// The start-failure policy: a failure the kubelet will not get past fails the
// helper once it has persisted for the grace period (one blip is ridden out);
// a pod that never starts fails at the start timeout; a running one never
// times out.
func TestHelperProgressPolicy(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &helperProgress{startTimeout: 10 * time.Minute, failureGrace: 30 * time.Second, created: t0}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: helperContainerName}}}}

	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if _, done, _ := p.observe(pod, t0); done {
		t.Fatal("pending pod reported done")
	}
	if dl := p.deadline(); !dl.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("deadline = %v, want the start timeout", dl)
	}

	pod.Status = imagePullBackOff(`Back-off pulling image "registry.internal/pg:17"`)
	p.observe(pod, t0.Add(time.Minute))
	if dl := p.deadline(); !dl.Equal(t0.Add(time.Minute + 30*time.Second)) {
		t.Errorf("deadline = %v, want failure grace", dl)
	}
	if err := p.check(t0.Add(time.Minute + 10*time.Second)); err != nil {
		t.Errorf("check within grace = %v, want nil", err)
	}
	// the pull succeeds on the kubelet's retry: the episode is over
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: helperContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}}
	p.observe(pod, t0.Add(time.Minute+20*time.Second))
	if err := p.check(t0.Add(2 * time.Minute)); err != nil {
		t.Errorf("check after recovery = %v, want nil", err)
	}

	// a persistent failure fails after the grace, carrying the kubelet's words
	pod.Status = imagePullBackOff(`Back-off pulling image "registry.internal/pg:17"`)
	p.observe(pod, t0.Add(3*time.Minute))
	pod.Status = imagePullBackOff(`Back-off pulling image "registry.internal/pg:17" (again)`)
	p.observe(pod, t0.Add(3*time.Minute+20*time.Second))
	err := p.check(t0.Add(3*time.Minute + 30*time.Second))
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") || !strings.Contains(err.Error(), "registry.internal/pg:17") {
		t.Errorf("check after grace = %v, want the ImagePullBackOff reason", err)
	}

	// never started: fails at the start timeout
	q := &helperProgress{startTimeout: time.Minute, failureGrace: 30 * time.Second, created: t0}
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	q.observe(pod, t0)
	if err := q.check(t0.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "not started after 1m0s") {
		t.Errorf("start timeout check = %v", err)
	}

	// started: nothing times out any more, and justStarted fires once
	r := &helperProgress{startTimeout: time.Minute, failureGrace: 30 * time.Second, created: t0}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
	if _, _, js := r.observe(pod, t0); !js {
		t.Error("first running sighting did not report justStarted")
	}
	if _, _, js := r.observe(pod, t0.Add(time.Second)); js {
		t.Error("justStarted reported twice")
	}
	if !r.deadline().IsZero() || r.check(t0.Add(24*time.Hour)) != nil {
		t.Error("a running helper must not time out")
	}
	pod.Status.Phase = corev1.PodSucceeded
	if phase, done, _ := r.observe(pod, t0.Add(25*time.Hour)); !done || phase != corev1.PodSucceeded {
		t.Errorf("observe(Succeeded) = %q, %v", phase, done)
	}
}

// Branch start failures surface through exec (the engine polls pg_isready):
// the error must say why the container is not running.
func TestExecOutputReportsWhyPodIsNotRunning(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx := context.Background()
	createPendingPod(t, cs, "pgoverlay-br-pull", imagePullBackOff(`Back-off pulling image "ghcr.io/acme/postgres:17": 401 Unauthorized`))
	_, err := d.ExecOutput(ctx, "pgoverlay-br-pull", []string{"pg_isready"})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("ExecOutput = %v, want the ImagePullBackOff reason", err)
	}

	createPendingPod(t, cs, "pgoverlay-br-crash", corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: helperContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 40s restarting failed container",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}},
	})
	_, err = d.ExecOutput(ctx, "pgoverlay-br-crash", []string{"pg_isready"})
	if err == nil || !strings.Contains(err.Error(), "CrashLoopBackOff") || !strings.Contains(err.Error(), "last exit 1") {
		t.Errorf("ExecOutput = %v, want CrashLoopBackOff and the last exit", err)
	}
}

func TestPodNotRunning(t *testing.T) {
	running := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "postgres", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	if why := podNotRunning(running); why != "" {
		t.Errorf("running pod: %q, want empty", why)
	}
	// phase Running without container detail yet: let exec decide
	running.Status.ContainerStatuses = nil
	if why := podNotRunning(running); why != "" {
		t.Errorf("running pod without statuses: %q, want empty", why)
	}
	evicted := &corev1.Pod{
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node was low on resource: ephemeral-storage."},
	}
	if why := podNotRunning(evicted); !strings.Contains(why, "Failed") || !strings.Contains(why, "Evicted") || !strings.Contains(why, "ephemeral-storage") {
		t.Errorf("evicted pod: %q", why)
	}
	fresh := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}}}
	if why := podNotRunning(fresh); why != "phase Pending" {
		t.Errorf("fresh pod: %q, want phase Pending", why)
	}
}

// Reconcile removes orphaned helper pods through StopRemove; their env
// Secret goes with them.
func TestStopRemoveHelperDeletesItsSecret(t *testing.T) {
	d, cs := fakeKubeDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	meta := helperObjectMeta("default", "pgoverlay-helper-orph1", "inst-1", nil)
	if _, err := cs.CoreV1().Secrets("default").Create(ctx, buildHelperSecret(meta, []string{"PGPASSWORD=x"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods("default").Create(ctx, buildHelperPod(meta, d.storage, HelperSpec{Image: UtilityImage}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.StopRemove(ctx, "pgoverlay-helper-orph1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets("default").Get(ctx, "pgoverlay-helper-orph1", metav1.GetOptions{}); err == nil {
		t.Error("orphaned helper's env Secret survived StopRemove")
	}
	// branch pods have no Secret: no delete is attempted for them
	before := len(cs.Actions())
	if err := d.StopRemove(ctx, "pgoverlay-br-x"); err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions()[before:] {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("StopRemove of a branch pod touched secrets: %v", a)
		}
	}
}
