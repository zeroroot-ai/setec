// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package deviceplugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const kvm = "setec.zeroroot.ai/kvm"

func mustNew(t *testing.T, statPath string, count int) *Plugin {
	t.Helper()
	p, err := New(Device{Resource: kvm, HostPath: "/dev/kvm", Count: count}, statPath, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNew_RefusesABadDevice(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]Device{
		"no resource":    {HostPath: "/dev/kvm", Count: 1},
		"relative path":  {Resource: kvm, HostPath: "dev/kvm", Count: 1},
		"count of zero":  {Resource: kvm, HostPath: "/dev/kvm"},
		"negative count": {Resource: kvm, HostPath: "/dev/kvm", Count: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(d, "", time.Second); err == nil {
				t.Fatalf("New(%+v) accepted the device", d)
			}
		})
	}
}

func TestAllocate_GivesTheHostDeviceOnly(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "/dev/null", 4)
	resp, err := p.Allocate(t.Context(), &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{
		{DevicesIds: []string{"0"}}, {DevicesIds: []string{"3"}},
	}})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(resp.ContainerResponses) != 2 {
		t.Fatalf("container responses = %d, want 2", len(resp.ContainerResponses))
	}
	for _, c := range resp.ContainerResponses {
		if len(c.Devices) != 1 || c.Devices[0].HostPath != "/dev/kvm" ||
			c.Devices[0].ContainerPath != "/dev/kvm" || c.Devices[0].Permissions != "rw" {
			t.Fatalf("device spec = %+v, want /dev/kvm rw", c.Devices)
		}
		if len(c.Mounts) != 0 || len(c.Envs) != 0 {
			t.Fatalf("the plugin must add no mount and no env, got %+v", c)
		}
	}
	if _, err := p.Allocate(t.Context(), &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{
		{DevicesIds: []string{"4"}},
	}}); err == nil {
		t.Fatal("Allocate accepted an ID the plugin never offered")
	}
}

// fakeStream collects the device lists that ListAndWatch sends.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan []*pluginapi.Device
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Send(r *pluginapi.ListAndWatchResponse) error {
	f.sent <- r.Devices
	return nil
}

func health(devs []*pluginapi.Device) string {
	if len(devs) == 0 {
		return "none"
	}
	return devs[0].Health
}

// TestListAndWatch_FollowsTheDevice proves that the advertised devices are
// unhealthy while the device is missing and healthy once it exists.
func TestListAndWatch_FollowsTheDevice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "kvm")
	p := mustNew(t, link, 3)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &fakeStream{ctx: ctx, sent: make(chan []*pluginapi.Device, 4)}
	done := make(chan error, 1)
	go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, s) }()

	first := <-s.sent
	if len(first) != 3 || health(first) != pluginapi.Unhealthy {
		t.Fatalf("first list = %d devices, %s; want 3 unhealthy", len(first), health(first))
	}
	// A character device appears at the path.
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	select {
	case next := <-s.sent:
		if health(next) != pluginapi.Healthy {
			t.Fatalf("after the device appeared: %s, want Healthy", health(next))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no new device list after the device appeared")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListAndWatch: %v", err)
	}
}

// fakeKubelet records the registration of a plugin.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	got chan *pluginapi.RegisterRequest
}

func (k *fakeKubelet) Register(_ context.Context, r *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	k.got <- r
	return &pluginapi.Empty{}, nil
}

func TestServe_RegistersWithTheKubelet(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kubeletSock := filepath.Join(dir, "kubelet.sock")
	lis, err := net.Listen("unix", kubeletSock)
	if err != nil {
		t.Fatal(err)
	}
	k := &fakeKubelet{got: make(chan *pluginapi.RegisterRequest, 1)}
	srv := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(srv, k)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	p := mustNew(t, "/dev/null", 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sock := filepath.Join(dir, "setec-kvm.sock")
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, sock, kubeletSock) }()

	var req *pluginapi.RegisterRequest
	select {
	case req = <-k.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin did not register")
	}
	if req.ResourceName != kvm || req.Endpoint != "setec-kvm.sock" || req.Version != pluginapi.Version {
		t.Fatalf("register request = %+v", req)
	}
	// The kubelet dials the plugin back on its socket.
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := pluginapi.NewDevicePluginClient(conn).Allocate(t.Context(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"1"}}},
	})
	if err != nil || resp.ContainerResponses[0].Devices[0].HostPath != "/dev/kvm" {
		t.Fatalf("Allocate over the socket: %v %+v", err, resp)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
