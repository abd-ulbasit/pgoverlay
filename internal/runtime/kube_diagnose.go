package runtime

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fatalWaitingReasons are container waiting reasons that will not clear on
// their own: waiting out the readiness timeout only delays the error.
var fatalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"CrashLoopBackOff":           true,
}

// DiagnoseContainer explains why a branch pod is not serving yet: its
// scheduling condition, the container's waiting reason, or its last
// termination, plus the tail of its logs when it crashed. fatal reports a
// state that will not resolve by waiting (image pull back-off, invalid
// image, bad container config, crash loop). The engine's readiness wait
// consults it (an optional driver capability) so a hopeless start fails fast
// with its cause, and the cause lands in the error before the compensation
// deletes the pod.
func (d *KubeDriver) DiagnoseContainer(ctx context.Context, id string) (string, bool) {
	pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	reason, fatal, crashed := diagnosePod(pod)
	if reason != "" && crashed {
		if logs := strings.TrimSpace(d.podLogs(ctx, id)); logs != "" {
			reason += "; last log lines:\n" + logs
		}
	}
	return reason, fatal
}

// diagnosePod is DiagnoseContainer's pure half. crashed reports that the
// container ran and exited, so its logs are worth fetching.
func diagnosePod(pod *corev1.Pod) (reason string, fatal, crashed bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("pod %s not scheduled: %s: %s", pod.Name, c.Reason, c.Message), false, false
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			msg := fmt.Sprintf("pod %s container %s waiting: %s", pod.Name, cs.Name, w.Reason)
			if w.Message != "" {
				msg += ": " + w.Message
			}
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg += fmt.Sprintf(" (last exit %d: %s)", t.ExitCode, t.Reason)
				crashed = true
			}
			return msg, fatalWaitingReasons[w.Reason], crashed
		}
		if t := cs.State.Terminated; t != nil {
			return fmt.Sprintf("pod %s container %s exited %d: %s %s", pod.Name, cs.Name, t.ExitCode, t.Reason, t.Message), false, true
		}
	}
	return "", false, false
}
