// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/errwrap"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// baseRetryFirst is the wait after the first failure of a base. Each
	// further failure doubles it, up to baseRetryMax.
	baseRetryFirst = 2 * time.Minute
	baseRetryMax   = 30 * time.Minute
	// reasonBaseFailing and reasonBaseBuilt are the reasons of the condition
	// WarmBaseFailing.
	reasonBaseFailing = "BaseSnapshotFailed"
	reasonBaseBuilt   = "BaseBuilt"
)

// baseFailure is the record of the bases of one key that failed in a row.
type baseFailure struct {
	count int
	until time.Time
	image string
	msg   string
}

// baseBackoff holds the failures of the bases of a pool. A base that cannot be
// made, for example a snapshot that the secret scan refuses, would otherwise
// start again as soon as the failed one goes, in a loop that boots a machine
// and scans its memory each time. A failure now holds the key back for a wait
// that doubles, and the class says why. The record is in memory: an operator
// that restarts tries once more.
type baseBackoff struct {
	mu sync.Mutex
	by map[string]*baseFailure
}

// failed records a failure of the base of key at now.
func (b *baseBackoff) failed(key, image string, err error, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.by == nil {
		b.by = map[string]*baseFailure{}
	}
	f := b.by[key]
	if f == nil {
		f = &baseFailure{}
		b.by[key] = f
	}
	f.count++
	wait := baseRetryFirst << min(f.count-1, 8)
	f.until = now.Add(min(wait, baseRetryMax))
	f.image = image
	f.msg = err.Error()
}

// built clears the record of key: its base is Ready.
func (b *baseBackoff) built(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.by, key)
}

// held reports whether the base of key waits for its next attempt.
func (b *baseBackoff) held(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.by[key]
	return f != nil && now.Before(f.until)
}

// forget drops each record whose key is not in keys.
func (b *baseBackoff) forget(keys map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.by {
		if _, ok := keys[k]; !ok {
			delete(b.by, k)
		}
	}
}

// summary says why the bases of the keys fail, or "" when none does.
func (b *baseBackoff) summary(keys map[string]string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var parts []string
	for k := range keys {
		if f := b.by[k]; f != nil {
			parts = append(parts, fmt.Sprintf("the base of %s failed %d times and waits until %s: %s",
				f.image, f.count, f.until.UTC().Format(time.RFC3339), f.msg))
		}
	}
	return strings.Join(parts, "; ")
}

// setBaseCondition writes the condition WarmBaseFailing of cls.
func (r *WarmPoolReconciler) setBaseCondition(ctx context.Context, cls *setecv1alpha1.SandboxClass, failing bool, msg string) error {
	status, reason := metav1.ConditionFalse, reasonBaseBuilt
	if failing {
		status, reason = metav1.ConditionTrue, reasonBaseFailing
	} else if meta.FindStatusCondition(cls.Status.Conditions, setecv1alpha1.ConditionWarmBaseFailing) == nil {
		return nil
	}
	if msg == "" {
		msg = "no base of the pool has failed"
	}
	orig := cls.DeepCopy()
	if !meta.SetStatusCondition(&cls.Status.Conditions, metav1.Condition{
		Type: setecv1alpha1.ConditionWarmBaseFailing, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: cls.Generation,
	}) {
		return nil
	}
	return errwrap.Wrap(r.Status().Patch(ctx, cls, client.MergeFrom(orig)), "client.SubResourceWriter.Patch")
}
