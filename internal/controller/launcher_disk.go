// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
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

// DiskBuilderConfig is how the operator runs setec-disk-builder: a build Job
// and a publish Job for each image digest, in the namespace of the operator
// (docs/design/storage.md). The operator signs between the two. No Job holds
// the signing seed.
type DiskBuilderConfig struct {
	// Image is the setec-disk-builder image.
	Image string
	// Namespace is the namespace of the operator, where the Jobs run.
	Namespace string
	// SigningSecret holds the ed25519 seed under the key "seed". Only the
	// operator reads it.
	SigningSecret string
	// ImageRegistries are the registry hosts that a launcher image may come
	// from. The chart policy of the build Job reaches these hosts and the
	// disk registry, and nothing else. The operator refuses an image of
	// another registry, so no build waits on a host that it cannot reach.
	ImageRegistries []string
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

	diskJobPrefix        = "setec-disk-"
	diskPublishJobPrefix = "setec-disk-publish-"
	// The component labels of the Job pods. The chart policy selects each.
	componentDiskBuilder   = "disk-builder"
	componentDiskPublisher = "disk-publisher"
	componentImageVerify   = "image-verify"
	// builderContainer is the one container of each Job pod.
	builderContainer = "disk-builder"
	// builderWorkDir is the emptyDir of a disk builder Job.
	builderWorkDir    = "/work"
	diskBuildRequeue  = 5 * time.Second
	diskJobTTLSeconds = 600
)

// diskJobName names the one build Job of an image digest and a signing key.
// A new signing seed has a new key id, so it gets a new Job and a new disk
// (setec#272).
func diskJobName(image, keyID string) (string, error) {
	_, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || len(digest) < 20 {
		return "", fmt.Errorf("a launcher Sandbox needs an image with a digest, got %q", image)
	}
	return diskJobPrefix + digest[:20] + "-" + keyID, nil
}

// diskPublishJobName names the publish Job that follows the build Job of
// image and keyID.
func diskPublishJobName(image, keyID string) (string, error) {
	name, err := diskJobName(image, keyID)
	if err != nil {
		return "", err
	}
	return diskPublishJobPrefix + strings.TrimPrefix(name, diskJobPrefix), nil
}

// errImageRegistryNotAllowed is a launcher image of a registry that is not
// in --image-registry. The build Job cannot reach it.
var errImageRegistryNotAllowed = errors.New("the registry of the image is not in --image-registry")

// checkImageRegistry refuses an image whose registry host is not one of
// allowed.
func checkImageRegistry(allowed []string, image string) error {
	d, err := diskbuilder.ParseDigestRef(image)
	if err != nil {
		return err //nolint:wrapcheck // the error names the image
	}
	host := d.RegistryStr()
	for _, a := range allowed {
		// The name form of a registry: docker.io is index.docker.io.
		reg, err := name.NewRegistry(strings.TrimSpace(a))
		if err == nil && strings.EqualFold(reg.RegistryStr(), host) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", errImageRegistryNotAllowed, host)
}

// errSigningKeyNotTrusted is a signing seed whose public key is not in
// --disk-public-key. Each launcher would refuse each disk that it signs.
var errSigningKeyNotTrusted = errors.New("the public key of the disk signing seed is not in --disk-public-key")

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get

// signingKey reads the signing seed and returns the key and its id, which
// names the disk tag (setec#272). The operator reads the seed on each disk
// step, so a rotated seed takes effect without a restart. A seed whose
// public key the launchers do not trust is refused. The operator is the only
// holder of the seed: it signs the report of the build Job, and no Job pod
// mounts the seed.
func signingKey(
	ctx context.Context, reader client.Reader, cfg DiskBuilderConfig, diskKeys []string,
) (ed25519.PrivateKey, string, error) {
	secret := &corev1.Secret{}
	ref := types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.SigningSecret}
	if err := reader.Get(ctx, ref, secret); err != nil {
		return nil, "", fmt.Errorf("read the disk signing Secret %s/%s: %w", cfg.Namespace, cfg.SigningSecret, err)
	}
	key, err := diskbuilder.ParsePrivateKey(secret.Data["seed"])
	if err != nil {
		return nil, "", fmt.Errorf("the disk signing Secret %s/%s, key seed: %w", cfg.Namespace, cfg.SigningSecret, err)
	}
	pub, _ := key.Public().(ed25519.PublicKey)
	encoded := diskbuilder.EncodePublicKey(pub)
	for _, k := range diskKeys {
		if strings.TrimSpace(k) == encoded {
			return key, diskbuilder.KeyID(pub), nil
		}
	}
	return nil, "", fmt.Errorf("%w (key id %s)", errSigningKeyNotTrusted, diskbuilder.KeyID(pub))
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
// signs, and keyID names it. The step has three parts:
//
//  1. The build Job pulls the image and pushes the disk layer. It has the
//     network of the image registries and no seed. Its termination message
//     is a diskbuilder.BuildResult.
//  2. The operator reads that report and signs the image digest and the
//     SHA-256 of the disk. The operator has no egress to a registry.
//  3. The publish Job pushes the signature and the tag. It reaches only the
//     disk registry and has no seed.
func ensureDisk(
	ctx context.Context, c client.Client, cfg DiskBuilderConfig, diskRepo string, diskKeys []string, image string,
) (keyID string, done bool, err error) {
	if cfg.Image == "" || cfg.Namespace == "" || cfg.SigningSecret == "" {
		return "", false, errors.New("the launcher backend needs the disk builder image, namespace and signing Secret")
	}
	if err := checkImageRegistry(cfg.ImageRegistries, image); err != nil {
		return "", false, &diskBuildError{msg: err.Error()}
	}
	reader := cfg.Reader
	if reader == nil {
		reader = c
	}
	key, keyID, err := signingKey(ctx, reader, cfg, diskKeys)
	if err != nil {
		return "", false, err
	}
	buildName, err := diskJobName(image, keyID)
	if err != nil {
		return "", false, err
	}
	state, err := jobState(ctx, reader, cfg.Namespace, buildName)
	if err != nil || state != jobComplete {
		return keyID, false, startOrWait(ctx, c, state, err, buildName, func() *batchv1.Job {
			return diskJob(cfg, diskRepo, buildName, image, keyID)
		})
	}
	res, err := buildResult(ctx, reader, cfg.Namespace, buildName)
	if err != nil {
		return "", false, &diskBuildError{job: buildName, msg: err.Error()}
	}
	if res.Exists {
		return keyID, true, nil
	}

	publishName, err := diskPublishJobName(image, keyID)
	if err != nil {
		return "", false, err
	}
	state, err = jobState(ctx, reader, cfg.Namespace, publishName)
	if err != nil || state != jobComplete {
		return keyID, false, startOrWait(ctx, c, state, err, publishName, func() *batchv1.Job {
			d, _ := diskbuilder.ParseDigestRef(image) // checked by checkImageRegistry
			sig := diskbuilder.SignDigest(key, d.DigestStr(), res.DiskSHA256)
			return publishJob(cfg, diskRepo, publishName, image, keyID, res, sig)
		})
	}
	return keyID, true, nil
}

// jobPhase is the state of a disk Job.
type jobPhase int

const (
	jobMissing jobPhase = iota
	jobRunning
	jobComplete
	jobFailedPhase
)

// jobFailure carries the message of a failed Job from jobState.
type jobFailure struct{ msg string }

func (e *jobFailure) Error() string { return e.msg }

// jobState reads the Job name in namespace.
func jobState(ctx context.Context, reader client.Reader, namespace, name string) (jobPhase, error) {
	job := &batchv1.Job{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, job)
	if apierrors.IsNotFound(err) {
		return jobMissing, nil
	}
	if err != nil {
		return jobMissing, errwrap.Wrap(err, "client.Reader.Get")
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return jobComplete, nil
		case batchv1.JobFailed:
			return jobFailedPhase, &jobFailure{msg: cond.Message}
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Not an end: JobComplete or JobFailed follows.
		}
	}
	return jobRunning, nil
}

// startOrWait creates the Job of newJob when it is missing, and turns a
// failed Job into a diskBuildError. A running Job is a wait.
func startOrWait(
	ctx context.Context, c client.Client, state jobPhase, stateErr error, name string, newJob func() *batchv1.Job,
) error {
	var failure *jobFailure
	switch {
	case errors.As(stateErr, &failure):
		return &diskBuildError{job: name, msg: failure.msg}
	case stateErr != nil:
		return stateErr
	case state == jobMissing:
		if err := c.Create(ctx, newJob()); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the disk Job %s: %w", name, err)
		}
	}
	return nil
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

// buildResult reads the report of the complete build Job name from the
// termination message of its Pod. The report is strict JSON: the operator
// signs what it names.
func buildResult(ctx context.Context, reader client.Reader, namespace, name string) (diskbuilder.BuildResult, error) {
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{batchv1.JobNameLabel: name}); err != nil {
		return diskbuilder.BuildResult{}, errwrap.Wrap(err, "client.Reader.List")
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodSucceeded {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == builderContainer && cs.State.Terminated != nil {
				return diskbuilder.ParseBuildResult([]byte(cs.State.Terminated.Message)) //nolint:wrapcheck // names the field
			}
		}
	}
	return diskbuilder.BuildResult{}, fmt.Errorf("the build Job %s has no Pod with a build result", name)
}

type diskBuildError struct{ job, msg string }

func (e *diskBuildError) Error() string {
	if e.job == "" {
		return "the disk of the image cannot be built: " + e.msg
	}
	return fmt.Sprintf("the disk Job %s failed: %s", e.job, e.msg)
}

// diskJob is the build Job of the disk of image for the key keyID. It has
// the push credential of the disk registry, and no signing seed.
func diskJob(cfg DiskBuilderConfig, diskRepo, name, image, keyID string) *batchv1.Job {
	return builderJob(cfg, name, componentDiskBuilder, []string{"--disk-repo", diskRepo,
		"--key-id", keyID, "--temp-dir", builderWorkDir, "build", image}, nil, true)
}

// publishJob is the publish Job of the disk of image: it gets the build
// report and the signature of the operator in its environment. It has the
// push credential of the disk registry, and no signing seed.
func publishJob(
	cfg DiskBuilderConfig, diskRepo, name, image, keyID string, res diskbuilder.BuildResult, sig diskbuilder.Signature,
) *batchv1.Job {
	resJSON, _ := json.Marshal(res) //nolint:errchkjson // plain strings and an int
	sigJSON, _ := json.Marshal(sig) //nolint:errchkjson // plain strings and bytes
	env := []corev1.EnvVar{
		{Name: "SETEC_DISK_BUILD", Value: string(resJSON)},
		{Name: "SETEC_DISK_SIGNATURE", Value: string(sigJSON)},
	}
	return builderJob(cfg, name, componentDiskPublisher, []string{"--disk-repo", diskRepo,
		"--key-id", keyID, "--temp-dir", builderWorkDir, "publish", image}, env, true)
}

// builderJob is a Job of the setec-disk-builder image with args. push
// mounts the push credential of the disk registry. No Job mounts the
// signing seed. The Job runs as a user that is not root, with no
// ServiceAccount token, and keeps its files in an emptyDir with a size limit.
func builderJob(cfg DiskBuilderConfig, name, component string, args []string, env []corev1.EnvVar, push bool) *batchv1.Job {
	ttl := int32(diskJobTTLSeconds)
	backoff := int32(2)
	work := resource.MustParse("20Gi")
	vols := []corev1.Volume{{Name: "work", EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &work}}}
	mounts := []corev1.VolumeMount{{Name: "work", MountPath: builderWorkDir}}
	if push && cfg.RegistrySecret != "" {
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
						Name:  builderContainer,
						Image: cfg.Image,
						Args:  args,
						Env:   env,
						// The build result goes to the termination message.
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
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
