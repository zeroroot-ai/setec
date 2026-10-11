// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;create

// DiskBuilderConfig is how the operator runs setec-disk-builder: one Job for
// each image digest, in the namespace of the operator (docs/design/storage.md).
type DiskBuilderConfig struct {
	// Image is the setec-disk-builder image.
	Image string
	// Namespace is the namespace of the operator, where the Jobs run.
	Namespace string
	// SigningSecret holds the ed25519 seed under the key "seed".
	SigningSecret string
	// RegistrySecret, when set, is a kubernetes.io/dockerconfigjson Secret
	// with the push credentials of the disk repository.
	RegistrySecret string
	// Reader reads the Jobs straight from the API server. The cache of the
	// manager would need list and watch on Jobs in every namespace; the
	// operator holds get and create in its own namespace only.
	Reader client.Reader
}

// Reasons of a launcher Sandbox that waits for or lost its disk.
const (
	ReasonDiskBuilding    = "DiskBuilding"
	ReasonDiskBuildFailed = "DiskBuildFailed"

	diskJobPrefix = "setec-disk-"
	// builderWorkDir is the emptyDir of a disk builder Job.
	builderWorkDir    = "/work"
	diskBuildRequeue  = 5 * time.Second
	diskJobTTLSeconds = 600
)

// diskJobName names the one Job of an image digest and a signing key. A new
// signing seed has a new key id, so it gets a new Job and a new disk
// (setec#272).
func diskJobName(image, keyID string) (string, error) {
	_, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || len(digest) < 20 {
		return "", fmt.Errorf("a launcher Sandbox needs an image with a digest, got %q", image)
	}
	return diskJobPrefix + digest[:20] + "-" + keyID, nil
}

// errSigningKeyNotTrusted is a signing seed whose public key is not in
// --disk-public-key. Each launcher would refuse each disk that it signs.
var errSigningKeyNotTrusted = errors.New("the public key of the disk signing seed is not in --disk-public-key")

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get

// signingKeyID reads the signing seed of the disk builder and returns the
// id of its key, which names the disk tag (setec#272). The operator reads
// the seed on each disk step, so a rotated seed takes effect without a
// restart. A seed whose public key the launchers do not trust is refused.
func signingKeyID(ctx context.Context, reader client.Reader, cfg DiskBuilderConfig, diskKeys []string) (string, error) {
	secret := &corev1.Secret{}
	ref := types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.SigningSecret}
	if err := reader.Get(ctx, ref, secret); err != nil {
		return "", fmt.Errorf("read the disk signing Secret %s/%s: %w", cfg.Namespace, cfg.SigningSecret, err)
	}
	key, err := diskbuilder.ParsePrivateKey(secret.Data["seed"])
	if err != nil {
		return "", fmt.Errorf("the disk signing Secret %s/%s, key seed: %w", cfg.Namespace, cfg.SigningSecret, err)
	}
	pub, _ := key.Public().(ed25519.PublicKey)
	encoded := diskbuilder.EncodePublicKey(pub)
	for _, k := range diskKeys {
		if strings.TrimSpace(k) == encoded {
			return diskbuilder.KeyID(pub), nil
		}
	}
	return "", fmt.Errorf("%w (key id %s)", errSigningKeyNotTrusted, diskbuilder.KeyID(pub))
}

// ensureLauncherDisk makes sure that the signed disk of the image of sb
// exists before its launcher Pod starts. It returns done=true and the key id
// of the disk when the disk is there; otherwise the caller requeues. The
// builder itself skips a disk that the registry already holds, so a Job for
// such a disk ends at once.
func (r *SandboxReconciler) ensureLauncherDisk(
	ctx context.Context, sb *setecv1alpha1.Sandbox,
) (keyID string, done bool, err error) {
	return ensureDisk(ctx, r.Client, r.DiskBuilder, r.DiskRepo, r.DiskKeys, sb.Spec.Image)
}

// ensureDisk is the disk step of a launcher Pod of image: of a Sandbox, or
// of a warm pool base. The disk is the one that the current signing seed
// signs, and keyID names it.
func ensureDisk(
	ctx context.Context, c client.Client, cfg DiskBuilderConfig, diskRepo string, diskKeys []string, image string,
) (keyID string, done bool, err error) {
	if cfg.Image == "" || cfg.Namespace == "" || cfg.SigningSecret == "" {
		return "", false, errors.New("the launcher backend needs the disk builder image, namespace and signing Secret")
	}
	reader := cfg.Reader
	if reader == nil {
		reader = c
	}
	keyID, err = signingKeyID(ctx, reader, cfg, diskKeys)
	if err != nil {
		return "", false, err
	}
	name, err := diskJobName(image, keyID)
	if err != nil {
		return "", false, err
	}
	job := &batchv1.Job{}
	err = reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, job)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, diskJob(cfg, diskRepo, name, image, keyID)); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("create the disk builder Job %s: %w", name, err)
		}
		return keyID, false, nil
	}
	if err != nil {
		return "", false, errwrap.Wrap(err, "client.Reader.Get")
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return keyID, true, nil
		case batchv1.JobFailed:
			return "", false, &diskBuildError{job: name, msg: cond.Message}
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Not an end: JobComplete or JobFailed follows.
		}
	}
	return keyID, false, nil
}

type diskBuildError struct{ job, msg string }

func (e *diskBuildError) Error() string {
	return fmt.Sprintf("the disk builder Job %s failed: %s", e.job, e.msg)
}

// diskJob is the Job that builds the disk of image with the key keyID. It
// runs as a user that is not root, with no ServiceAccount token, and keeps
// the build in an emptyDir with a size limit. The builder refuses a seed of
// another key id, so the operator never waits for a tag that no build writes.
func diskJob(cfg DiskBuilderConfig, diskRepo, name, image, keyID string) *batchv1.Job {
	return builderJob(cfg, name, "disk-builder", []string{"--disk-repo", diskRepo, "--key-file", "/etc/setec/disk-signing/seed",
		"--key-id", keyID, "--temp-dir", builderWorkDir, "build", image}, nil, true)
}

// builderJob is a Job of the setec-disk-builder image with args. signing
// mounts the disk signing Secret. The Job runs as a user that is not
// root, with no ServiceAccount token, and keeps its files in an emptyDir
// with a size limit.
func builderJob(cfg DiskBuilderConfig, name, component string, args []string, env []corev1.EnvVar, signing bool) *batchv1.Job {
	ttl := int32(diskJobTTLSeconds)
	backoff := int32(2)
	work := resource.MustParse("20Gi")
	vols := []corev1.Volume{{Name: "work", EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &work}}}
	mounts := []corev1.VolumeMount{{Name: "work", MountPath: builderWorkDir}}
	if signing {
		vols = append(vols, corev1.Volume{Name: "signing", Secret: &corev1.SecretVolumeSource{SecretName: cfg.SigningSecret}})
		mounts = append(mounts, corev1.VolumeMount{Name: "signing", MountPath: "/etc/setec/disk-signing", ReadOnly: true})
	}
	if cfg.RegistrySecret != "" {
		vols = append(vols, corev1.Volume{Name: "registry", Secret: &corev1.SecretVolumeSource{
			SecretName: cfg.RegistrySecret,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "registry", MountPath: "/etc/setec/registry", ReadOnly: true})
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/etc/setec/registry"})
	}
	return &batchv1.Job{
		Name:      name,
		Namespace: cfg.Namespace,
		Labels:    map[string]string{"app.kubernetes.io/component": component},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Labels: map[string]string{"app.kubernetes.io/component": component},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
					Containers: []corev1.Container{{
						Name:  "disk-builder",
						Image: cfg.Image,
						Args:  args,
						Env:   env,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							ReadOnlyRootFilesystem:   new(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: mounts,
					}},
					Volumes: vols,
				},
			},
		},
	}
}

// waitForLauncherDisk is the createPod step of a launcher Sandbox. It
// returns proceed=true and the key id of the disk when the disk exists;
// otherwise the result to return from Reconcile.
func (r *SandboxReconciler) waitForLauncherDisk(
	ctx context.Context, sb *setecv1alpha1.Sandbox,
) (keyID string, proceed bool, res ctrl.Result, err error) {
	keyID, done, err := r.ensureLauncherDisk(ctx, sb)
	if err != nil {
		if _, failed := err.(*diskBuildError); failed { //nolint:errorlint // a local type, never wrapped
			if r.Recorder != nil {
				r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, ReasonDiskBuildFailed, actionFinalizeSandbox, "%s", err.Error())
			}
			return "", false, ctrl.Result{}, r.patchFailedStatus(ctx, sb, ReasonDiskBuildFailed)
		}
		res, err := r.recordAndReturnErr(sb, eventReasonPodCreateFailed, err)
		return "", false, res, err
	}
	if !done {
		if err := r.patchPendingStatus(ctx, sb, ReasonDiskBuilding); err != nil {
			return "", false, ctrl.Result{}, err
		}
		return "", false, ctrl.Result{RequeueAfter: diskBuildRequeue}, nil
	}
	return keyID, true, ctrl.Result{}, nil
}
