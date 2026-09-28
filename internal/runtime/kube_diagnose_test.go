package runtime

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDiagnosePod(t *testing.T) {
	waiting := func(reason, msg string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pgoverlay-br-pr-1"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "postgres", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}},
			}}}}
	}
	for _, tc := range []struct {
		name      string
		pod       *corev1.Pod
		want      string
		fatal     bool
		crashed   bool
		wantEmpty bool
	}{
		{name: "image pull back-off", pod: waiting("ImagePullBackOff", `Back-off pulling image "ghcr.io/acme/postgres:17"`),
			want: `ImagePullBackOff: Back-off pulling image "ghcr.io/acme/postgres:17"`, fatal: true},
		{name: "first pull error is not yet fatal", pod: waiting("ErrImagePull", "rpc error"), want: "ErrImagePull"},
		{name: "config error", pod: waiting("CreateContainerConfigError", `secret "x" not found`), want: "CreateContainerConfigError", fatal: true},
		{name: "still creating", pod: waiting("ContainerCreating", ""), wantEmpty: true},
		{name: "unschedulable", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available"}},
		}}, want: "not scheduled: Unschedulable: 0/3 nodes are available"},
		{name: "crash loop", pod: func() *corev1.Pod {
			p := waiting("CrashLoopBackOff", "back-off 20s restarting failed container")
			p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 32, Reason: "Error"}}
			return p
		}(), want: "CrashLoopBackOff", fatal: true, crashed: true},
		{name: "running", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, wantEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal, crashed := diagnosePod(tc.pod)
			if tc.wantEmpty {
				if reason != "" {
					t.Fatalf("reason=%q want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.want) || fatal != tc.fatal || crashed != tc.crashed {
				t.Fatalf("reason=%q fatal=%v crashed=%v, want %q fatal=%v crashed=%v", reason, fatal, crashed, tc.want, tc.fatal, tc.crashed)
			}
		})
	}
}

func TestKubeDiagnoseContainerReadsPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pgoverlay-br-pr-1", Namespace: "default"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "postgres", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName"}},
		}}}}
	d := &KubeDriver{cs: fake.NewSimpleClientset(pod), namespace: "default"}
	reason, fatal := d.DiagnoseContainer(context.Background(), "pgoverlay-br-pr-1")
	if !fatal || !strings.Contains(reason, "InvalidImageName") {
		t.Fatalf("reason=%q fatal=%v", reason, fatal)
	}
	if reason, fatal := d.DiagnoseContainer(context.Background(), "missing"); reason != "" || fatal {
		t.Fatalf("missing pod: reason=%q fatal=%v", reason, fatal)
	}
}
