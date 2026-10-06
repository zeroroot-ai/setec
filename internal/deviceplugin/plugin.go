// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package deviceplugin offers a host character device, such as /dev/kvm,
// to Pods as a Kubernetes extended resource. A launcher Pod asks for the
// resource and the kubelet puts the device into that container only. The
// Pod needs no privileged flag and no hostPath volume
// (docs/design/runtime.md).
//
// A host device has no capacity of its own: many Pods share /dev/kvm. The
// plugin therefore advertises Count virtual devices, each one a share of
// the same host path. The count caps how many launcher Pods a node takes.
package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Device names one host device and the resource that offers it.
type Device struct {
	// Resource is the extended resource name, e.g. setec.zeroroot.ai/kvm.
	Resource string
	// HostPath is the device on the node, e.g. /dev/kvm.
	HostPath string
	// ContainerPath is where the container sees it. Empty means HostPath.
	ContainerPath string
	// Count is the number of Pods that may hold the device at one time.
	Count int
}

// Plugin serves the device plugin API for one Device.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer

	dev Device
	// statPath is the device path as the plugin process sees it. It equals
	// HostPath when the plugin runs on the host, or a path under a mount.
	statPath string
	interval time.Duration
}

// New checks d and returns its Plugin. statPath is where this process can
// stat the device; the kubelet still hands HostPath to the container.
func New(d Device, statPath string, healthInterval time.Duration) (*Plugin, error) {
	switch {
	case d.Resource == "":
		return nil, errors.New("deviceplugin: the resource name is empty")
	case !filepath.IsAbs(d.HostPath):
		return nil, fmt.Errorf("deviceplugin: host path %q is not absolute", d.HostPath)
	case d.Count < 1:
		return nil, fmt.Errorf("deviceplugin: %s: the count must be at least 1, got %d", d.Resource, d.Count)
	case healthInterval <= 0:
		return nil, errors.New("deviceplugin: the health interval must be positive")
	}
	if d.ContainerPath == "" {
		d.ContainerPath = d.HostPath
	}
	if statPath == "" {
		statPath = d.HostPath
	}
	return &Plugin{dev: d, statPath: statPath, interval: healthInterval}, nil
}

// present reports whether the device exists and is a character device.
func (p *Plugin) present() bool {
	fi, err := os.Stat(p.statPath)
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// devices returns the advertised devices, all healthy or all unhealthy.
func (p *Plugin) devices(healthy bool) []*pluginapi.Device {
	state := pluginapi.Unhealthy
	if healthy {
		state = pluginapi.Healthy
	}
	out := make([]*pluginapi.Device, p.dev.Count)
	for i := range out {
		out[i] = &pluginapi.Device{ID: strconv.Itoa(i), Health: state}
	}
	return out
}

// GetDevicePluginOptions reports that the plugin needs no pre-start hook.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch sends the device list, then a new list each time the
// device appears or goes away. A node without the device advertises
// unhealthy devices, so the scheduler places no launcher Pod there.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, stream grpc.ServerStreamingServer[pluginapi.ListAndWatchResponse]) error {
	last := p.present()
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices(last)}); err != nil {
		return err
	}
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-tick.C:
			now := p.present()
			if now == last {
				continue
			}
			last = now
			if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices(now)}); err != nil {
				return err
			}
		}
	}
}

// Allocate gives each container that asked for the resource the one host
// device, read and write. It refuses an ID that the plugin never offered.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := &pluginapi.AllocateResponse{}
	for _, c := range req.GetContainerRequests() {
		for _, id := range c.GetDevicesIds() {
			n, err := strconv.Atoi(id)
			if err != nil || n < 0 || n >= p.dev.Count {
				return nil, fmt.Errorf("deviceplugin: %s: unknown device ID %q", p.dev.Resource, id)
			}
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: []*pluginapi.DeviceSpec{{
				ContainerPath: p.dev.ContainerPath,
				HostPath:      p.dev.HostPath,
				Permissions:   "rw",
			}},
		})
	}
	return resp, nil
}

// GetPreferredAllocation has no preference: every share is the same.
func (p *Plugin) GetPreferredAllocation(
	context.Context, *pluginapi.PreferredAllocationRequest,
) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// PreStartContainer is not used.
func (p *Plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// Serve listens on socket (in the kubelet device plugin directory),
// registers the resource with the kubelet at kubeletSocket, and serves
// until ctx ends or the listener fails. The kubelet deletes the socket
// when it restarts; the caller then calls Serve again.
func (p *Plugin) Serve(ctx context.Context, socket, kubeletSocket string) error {
	_ = os.Remove(socket)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("deviceplugin: listen on %s: %w", socket, err)
	}
	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	defer srv.Stop()

	if err := register(ctx, kubeletSocket, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     filepath.Base(socket),
		ResourceName: p.dev.Resource,
		Options:      &pluginapi.DevicePluginOptions{},
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func register(ctx context.Context, kubeletSocket string, req *pluginapi.RegisterRequest) error {
	conn, err := grpc.NewClient("unix://"+kubeletSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("deviceplugin: dial the kubelet at %s: %w", kubeletSocket, err)
	}
	defer func() { _ = conn.Close() }()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := pluginapi.NewRegistrationClient(conn).Register(rctx, req); err != nil {
		return fmt.Errorf("deviceplugin: register %s with the kubelet: %w", req.ResourceName, err)
	}
	return nil
}
