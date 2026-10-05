// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-device-plugin offers /dev/kvm and /dev/net/tun of a node to
// launcher Pods as the extended resources setec.zeroroot.ai/kvm and
// setec.zeroroot.ai/tun (docs/design/runtime.md). It runs as one DaemonSet
// Pod on each fleet node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/zeroroot-ai/setec/internal/deviceplugin"
)

func main() {
	var (
		pluginDir = flag.String("plugin-dir", pluginapi.DevicePluginPath,
			"the kubelet device plugin directory, mounted from the node")
		count = flag.Int("count", 110,
			"how many launcher Pods a node may run at one time; each takes one share of each device")
		health = flag.Duration("health-interval", 10*time.Second, "how often to check that each device exists")
	)
	flag.Parse()

	devices := []deviceplugin.Device{
		{Resource: "setec.zeroroot.ai/kvm", HostPath: "/dev/kvm", Count: *count},
		{Resource: "setec.zeroroot.ai/tun", HostPath: "/dev/net/tun", Count: *count},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, d := range devices {
		p, err := deviceplugin.New(d, d.HostPath, *health)
		if err != nil {
			fmt.Fprintf(os.Stderr, "setec-device-plugin: %v\n", err)
			os.Exit(1)
		}
		socket := filepath.Join(*pluginDir, "setec-"+filepath.Base(d.HostPath)+".sock")
		wg.Go(func() { run(ctx, p, d.Resource, socket, filepath.Join(*pluginDir, "kubelet.sock")) })
	}
	wg.Wait()
}

// run serves one plugin and serves it again when the kubelet restarts. A
// restarted kubelet deletes every plugin socket, so a missing socket means
// the plugin must register again.
func run(ctx context.Context, p *deviceplugin.Plugin, resource, socket, kubeletSocket string) {
	for ctx.Err() == nil {
		sctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- p.Serve(sctx, socket, kubeletSocket) }()
		err := waitForRestart(sctx, socket, done)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "setec-device-plugin: %s: %v\n", resource, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		fmt.Fprintf(os.Stderr, "setec-device-plugin: %s: registering again\n", resource)
	}
}

// waitForRestart returns when Serve ends or when the plugin socket is gone.
func waitForRestart(ctx context.Context, socket string, done <-chan error) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if _, err := os.Stat(socket); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("the plugin socket %s is gone; the kubelet restarted", socket)
			}
		}
	}
}
