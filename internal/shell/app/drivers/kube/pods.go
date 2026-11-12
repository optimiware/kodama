package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// Pod response bodies.

type podWire struct {
	Metadata objectMeta    `json:"metadata"`
	Spec     podSpecWire   `json:"spec"`
	Status   podStatusWire `json:"status"`
}

type podSpecWire struct {
	NodeName string `json:"nodeName"`
}

type podStatusWire struct {
	Phase             string                `json:"phase"`
	ContainerStatuses []containerStatusWire `json:"containerStatuses"`
}

type containerStatusWire struct {
	Name  string `json:"name"`
	State struct {
		Terminated *terminatedStateWire `json:"terminated"`
	} `json:"state"`
}

type terminatedStateWire struct {
	ExitCode int `json:"exitCode"`
}

type podListWire struct {
	Items []podWire `json:"items"`
}

// ListPods returns every Pod matching the selector. Phase and container status
// are observations only; they never prove a command stopped by themselves.
func (c *Cluster) ListPods(ctx context.Context, namespace string, labels map[string]string) ([]runtimekube.Pod, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, podsPath(namespace), labelSelector(labels), nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("list Pods", status, false)
	}
	var list podListWire
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, undecodable("list Pods")
	}
	pods := make([]runtimekube.Pod, 0, len(list.Items))
	for _, item := range list.Items {
		pod, err := item.pod("list Pods")
		if err != nil {
			return nil, err
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// podLogStatusError classifies one non-OK pod-log response without inspecting or
// rendering its body. A vanished pod stays NotFound so the caller can record an
// explicit absence marker; a rejected request stays a retryable dependency
// fault; an API-server failure keeps the existing uncertain-effect class. The
// sentinels let the adapter name the failed inner log operation.
func podLogStatusError(status int) error {
	switch {
	case status == http.StatusNotFound:
		return fault.Wrap(fault.NotFound, "kubernetes read Pod logs: object not found", runtimekube.ErrNotFound)
	case status >= 400 && status < 500:
		return fault.Wrap(fault.DependencyUnavailable,
			fmt.Sprintf("kubernetes read Pod logs rejected with status %d", status),
			runtimekube.ErrPodLogBadRequest)
	default:
		return ambiguous(
			fmt.Sprintf("kubernetes read Pod logs returned status %d", status),
			runtimekube.ErrPodLogServerError)
	}
}

// pod maps one Pod response onto the seam type.
func (wire podWire) pod(operation string) (runtimekube.Pod, error) {
	deleted, err := wire.Metadata.parseDeletionTimestamp()
	if err != nil {
		return runtimekube.Pod{}, undecodable(operation)
	}
	containers := make([]runtimekube.ContainerStatus, 0, len(wire.Status.ContainerStatuses))
	for _, container := range wire.Status.ContainerStatuses {
		status := runtimekube.ContainerStatus{Name: container.Name}
		if container.State.Terminated != nil {
			status.Terminated = true
			status.ExitCode = container.State.Terminated.ExitCode
		}
		containers = append(containers, status)
	}
	return runtimekube.Pod{
		Name:              wire.Metadata.Name,
		Namespace:         wire.Metadata.Namespace,
		Labels:            wire.Metadata.Labels,
		Phase:             wire.Status.Phase,
		NodeName:          wire.Spec.NodeName,
		Containers:        containers,
		DeletionTimestamp: deleted,
	}, nil
}

// ReadPodLogs reads one bounded page of the combined pod log. The offset is
// applied by discarding exactly that many bytes, so the caller's continuation
// cursor is one byte position in the interleaved stream.
func (c *Cluster) ReadPodLogs(
	ctx context.Context,
	namespace, pod string,
	request runtimekube.PodLogRequest,
) (runtimekube.PodLogPage, error) {
	if request.Limit <= 0 || request.Limit > runtimekube.MaxLogBytes {
		return runtimekube.PodLogPage{}, fault.New(fault.InvalidInput,
			"kubernetes log read limit is outside the bounded range")
	}
	if request.Offset < 0 {
		return runtimekube.PodLogPage{}, fault.New(fault.InvalidInput,
			"kubernetes log read offset is negative")
	}
	// The log API can separate the two streams only behind the alpha
	// PodLogsQuerySplitStreams gate, which stock clusters leave off and which
	// rejects the stream parameter otherwise. The one combined log is the
	// portable read; the seam names it "combined".
	if request.Stream != runtimekube.LogStreamCombined {
		return runtimekube.PodLogPage{}, fault.New(fault.InvalidInput,
			"kubernetes log read stream must be the combined log")
	}
	query := url.Values{}
	query.Set("container", workloadContainerName)
	query.Set("follow", "false")

	requestCtx, cancel := c.callContext(ctx)
	defer cancel()
	response, err := c.do(requestCtx, http.MethodGet, podLogPath(namespace, pod), query, nil, "")
	if err != nil {
		return runtimekube.PodLogPage{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, errorDrainBytes)
		return runtimekube.PodLogPage{}, podLogStatusError(response.StatusCode)
	}

	if request.Offset > 0 {
		if _, err := io.CopyN(io.Discard, response.Body, request.Offset); err != nil {
			if errors.Is(err, io.EOF) {
				// The stream ended before the requested offset: no progress,
				// and the caller can keep polling without inventing bytes.
				return runtimekube.PodLogPage{NextOffset: request.Offset}, nil
			}
			return runtimekube.PodLogPage{}, ambiguousRequest("read Pod logs", requestCtx, err)
		}
	}
	// Read one byte past the limit so truncation is observable instead of
	// silently losing the boundary.
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(request.Limit)+1))
	if err != nil {
		return runtimekube.PodLogPage{}, ambiguousRequest("read Pod logs", requestCtx, err)
	}
	truncated := len(data) > request.Limit
	if truncated {
		data = data[:request.Limit]
	}
	return runtimekube.PodLogPage{
		Data:       data,
		NextOffset: request.Offset + int64(len(data)),
		Truncated:  truncated,
	}, nil
}
