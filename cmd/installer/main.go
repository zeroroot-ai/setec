// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command installer is the portable node installer DaemonSet binary
// (ADR-0003, issue #187). It converges the node it runs on to boot
// kata-fc Firecracker microVMs — stock Kata payload, devmapper thin-pool
// with boot ordering, containerd registration — then idles, re-verifying
// on an interval. All convergence logic lives in internal/installer and
// is unit-tested without a real node; main.go handles only flags, signal
// handling, the readiness endpoint, and the retry loop.
//
// The installer deliberately has NO Kubernetes API access: the kata-fc
// RuntimeClass is rendered by the Helm chart and node capability labels
// are owned by the runtime-agent DaemonSet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zeroroot-ai/setec/internal/installer"
)

func main() {
	var (
		hostRoot     = flag.String("host-root", "/host", "mount point of the node's root filesystem inside this container")
		payloadDir   = flag.String("payload-dir", "/opt/kata", "kata static release payload bundled in this image")
		poolName     = flag.String("pool-name", "setec-thinpool", "devmapper thin-pool name")
		thinpoolMode = flag.String("thinpool-mode", installer.ThinpoolModeLoop,
			"thin-pool backing: loop (sparse files, portable default) or device (dedicated block devices)")
		loopDir = flag.String("loop-dir", "/var/lib/setec/thinpool",
			"host directory for sparse backing files (loop mode)")
		loopDataGB    = flag.Int("loop-data-gb", 50, "sparse data file size in GiB (loop mode)")
		loopMetaGB    = flag.Int("loop-meta-gb", 2, "sparse metadata file size in GiB (loop mode)")
		dataDevice    = flag.String("data-device", "", "dedicated data block device (device mode)")
		metaDevice    = flag.String("metadata-device", "", "dedicated metadata block device (device mode)")
		devmapperRoot = flag.String("devmapper-root",
			"/var/lib/containerd/io.containerd.snapshotter.v1.devmapper",
			"containerd devmapper snapshotter root_path")
		baseImageSize  = flag.String("base-image-size", "8589934592", "devmapper snapshotter base_image_size in bytes")
		verifyInterval = flag.Duration("verify-interval", 10*time.Minute, "how often to re-verify convergence")
		healthAddr     = flag.String("health-addr", ":8080", "listen address for the /healthz readiness endpoint")
	)
	flag.Parse()

	cfg := installer.Config{
		HostRoot:       *hostRoot,
		PayloadDir:     *payloadDir,
		PoolName:       *poolName,
		ThinpoolMode:   *thinpoolMode,
		LoopDir:        *loopDir,
		LoopDataGB:     *loopDataGB,
		LoopMetaGB:     *loopMetaGB,
		DataDevice:     *dataDevice,
		MetadataDevice: *metaDevice,
		DevmapperRoot:  *devmapperRoot,
		BaseImageSize:  *baseImageSize,
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	inst, err := installer.New(cfg, logger.Printf)
	if err != nil {
		logger.Printf("invalid configuration: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Readiness: 200 once the node is converged (or deliberately idle —
	// no KVM, foreign owner), 503 while convergence is failing. Wired to
	// the DaemonSet's readinessProbe only; no livenessProbe, so a node
	// that cannot converge shows NotReady instead of restart-looping.
	var ready atomic.Bool
	var lastState atomic.Value // string
	lastState.Store("starting")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		state, _ := lastState.Load().(string)
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = fmt.Fprintln(w, state)
	})
	srv := &http.Server{Addr: *healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("health server: %v", err)
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck // best-effort on exit

	// Converge, then re-verify forever. Failures retry with a shorter
	// backoff than the steady-state interval so a transient (image pull
	// on a slow disk, containerd busy) heals quickly.
	const failureRetry = 30 * time.Second
	for {
		res, err := inst.Converge(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			logger.Printf("shutting down")
			return
		case err != nil:
			ready.Store(false)
			lastState.Store("error: " + err.Error())
			logger.Printf("convergence failed (retrying in %s): %v", failureRetry, err)
		default:
			ready.Store(true)
			lastState.Store(string(res.Outcome))
			logger.Printf("outcome=%s changed=%t runtimeRestarted=%t flavor=%s",
				res.Outcome, res.Changed, res.RuntimeRestarted, res.Flavor)
		}

		wait := *verifyInterval
		if err != nil {
			wait = failureRetry
		}
		select {
		case <-ctx.Done():
			logger.Printf("shutting down")
			return
		case <-time.After(wait):
		}
	}
}
