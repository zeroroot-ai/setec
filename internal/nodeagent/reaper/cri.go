package reaper

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRIClient implements SandboxClient against a CRI runtime service exposed on a
// containerd Unix socket (the same endpoint crictl uses). containerd serves the
// CRI RuntimeService on its socket, so no separate endpoint is required.
type CRIClient struct {
	conn *grpc.ClientConn
	rt   runtimeapi.RuntimeServiceClient
}

// NewCRIClient dials the CRI runtime service on the given containerd Unix
// socket path (e.g. /run/containerd/containerd.sock or, on k3s,
// /run/k3s/containerd/containerd.sock).
func NewCRIClient(socketPath string) (*CRIClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial CRI socket %q: %w", socketPath, err)
	}
	return &CRIClient{conn: conn, rt: runtimeapi.NewRuntimeServiceClient(conn)}, nil
}

// Close releases the gRPC connection.
func (c *CRIClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ListNotReadySandboxes returns all pod sandboxes in the NOT_READY state.
func (c *CRIClient) ListNotReadySandboxes(ctx context.Context) ([]Sandbox, error) {
	resp, err := c.rt.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{
			State: &runtimeapi.PodSandboxStateValue{
				State: runtimeapi.PodSandboxState_SANDBOX_NOTREADY,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list pod sandboxes: %w", err)
	}
	out := make([]Sandbox, 0, len(resp.GetItems()))
	for _, item := range resp.GetItems() {
		sb := Sandbox{
			ID:             item.GetId(),
			RuntimeHandler: item.GetRuntimeHandler(),
		}
		if md := item.GetMetadata(); md != nil {
			sb.Name = md.GetName()
			sb.Namespace = md.GetNamespace()
		}
		// CreatedAt is nanoseconds since the Unix epoch; 0 means unreported.
		if ts := item.GetCreatedAt(); ts > 0 {
			sb.CreatedAt = time.Unix(0, ts)
		}
		out = append(out, sb)
	}
	return out, nil
}

// ForceRemove stops (best-effort) then removes the sandbox, mirroring
// `crictl rmp -f`. StopPodSandbox is idempotent on an already-stopped sandbox;
// its error is non-fatal because the leaked-VMM case is precisely the one where
// a graceful stop already failed — RemovePodSandbox still releases the
// containerd name reservation and tears down lingering resources.
func (c *CRIClient) ForceRemove(ctx context.Context, id string) error {
	_, _ = c.rt.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: id})
	if _, err := c.rt.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: id}); err != nil {
		return fmt.Errorf("remove pod sandbox %q: %w", id, err)
	}
	return nil
}
