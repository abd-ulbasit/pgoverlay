package runtime

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// IsInUse reports whether err, from a Driver call or a helper it ran, is the
// runtime refusing to remove something that is still in use: a docker volume
// a container still mounts (409 Conflict, "volume is in use"), or a busy zfs
// dataset or mount. Removing it again once that user is gone succeeds.
func IsInUse(err error) bool {
	if err == nil {
		return false
	}
	if cerrdefs.IsConflict(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "is in use") ||
		strings.Contains(msg, "dataset is busy") ||
		strings.Contains(msg, "resource busy")
}

// IsUnavailable reports whether err means the runtime could not be reached or
// did not answer in time: the Docker daemon is down or its socket is gone,
// the Kubernetes API server is unavailable, overloaded or timed out, a
// connection was refused, or the call ran out of time.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if client.IsErrConnectionFailed(err) || cerrdefs.IsUnavailable(err) {
		return true
	}
	if apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
