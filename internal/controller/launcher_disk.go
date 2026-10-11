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

	ggcrname "github.com/google/go-containerregistry/pkg/name"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;create

// DiskBuilderConfig is how the operator runs setec-disk-builder: one Job for
// each image digest and signing key, in the namespace of the operator
// (docs/design/runtime.md). The Job has three containers: check, build and
// publish. The operator signs between build and publish. No container holds
// the signing seed, and the build container holds no secret at all.
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

	diskJobPrefix = "setec-disk-"
	// The component labels of the Job pods. The chart policy selects each.
	componentDiskBuilder = "disk-builder"
	componentImageVerify = "image-verify"
	// The containers of a disk Job, in order: check and build are init
	// containers, publish is the main one. The verify Job has one container,
	// named like the build container.
	checkContainer   = "check"
	builderContainer = "disk-builder"
	publishContainer = "publish"
	// tempDirFlag names the temporary directory of setec-disk-builder.
	tempDirFlag = "--temp-dir"
	// signatureDir is where the publish container finds the signature.
	signatureDir = "/etc/setec/disk-signature"
	// signatureKey is the key of the signature in its ConfigMap.
	signatureKey = "signature.json"
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
		reg, err := ggcrname.NewRegistry(strings.TrimSpace(a))
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
// signs, and keyID names it. One Job makes the disk:
//
//  1. check: looks up the disk with the push credential. When the registry
//     holds it, the Job ends with no build.
//  2. build: pulls the image and makes the disk. It holds no secret. It
//     reports the SHA-256 of the disk in its termination message.
//  3. publish: waits for the signature and pushes the disk and the
//     signature with the push credential.
//
// Between build and publish the operator reads the report, signs the image
// digest and the disk SHA-256, and writes the signature to a ConfigMap that
// the publish container mounts. The operator has no registry egress.
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
	jobName, err := diskJobName(image, keyID)
	if err != nil {
		return "", false, err
	}
	job := &batchv1.Job{}
	err = reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}, job)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, diskJob(cfg, diskRepo, jobName, image, keyID)); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("create the disk Job %s: %w", jobName, err)
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
			return "", false, &diskBuildError{job: jobName, msg: cond.Message}
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Not an end: JobComplete or JobFailed follows.
		}
	}
	if err := signBuiltDisk(ctx, c, reader, cfg, job, image, key); err != nil {
		return "", false, err
	}
	return keyID, false, nil
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;create

// signBuiltDisk signs the disk of the running Job job once its build
// container has reported, and writes the signature ConfigMap that the
// publish container waits for. It does nothing while the build runs, when
// the check found the disk, or when the ConfigMap exists. A report that is
// not valid fails the disk: the operator signs only what it can read.
func signBuiltDisk(
	ctx context.Context, c client.Client, reader client.Reader, cfg DiskBuilderConfig,
	job *batchv1.Job, image string, key ed25519.PrivateKey,
) error {
	cmName := job.Name + "-sig"
	err := reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: cmName}, &corev1.ConfigMap{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errwrap.Wrap(err, "client.Reader.Get")
	}
	checkMsg, buildMsg, err := initReports(ctx, reader, cfg.Namespace, job.Name)
	if err != nil || buildMsg == "" {
		return err
	}
	if checkMsg != "" {
		check, err := diskbuilder.ParseCheckResult([]byte(checkMsg))
		if err != nil {
			return &diskBuildError{job: job.Name, msg: err.Error()}
		}
		if check.Exists {
			return nil
		}
	}
	res, err := diskbuilder.ParseBuildResult([]byte(buildMsg))
	if err != nil {
		return &diskBuildError{job: job.Name, msg: err.Error()}
	}
	d, err := diskbuilder.ParseDigestRef(image)
	if err != nil {
		return err //nolint:wrapcheck // names the image
	}
	sig, err := json.Marshal(diskbuilder.SignDigest(key, d.DigestStr(), res.DiskSHA256))
	if err != nil {
		return errwrap.Wrap(err, "json.Marshal")
	}
	ctrlTrue := true
	cm := &corev1.ConfigMap{
		Name: cmName, Namespace: cfg.Namespace,
		Labels: map[string]string{"app.kubernetes.io/component": componentDiskBuilder},
		// The Job owns the ConfigMap, so it goes with the Job.
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &ctrlTrue,
		}},
		Data: map[string]string{signatureKey: string(sig)},
	}
	if err := c.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the disk signature ConfigMap %s: %w", cmName, err)
	}
	return nil
}

// initReports returns the termination messages of the check and the build
// container of the Pod of the Job jobName, once the build container has
// ended with success. Both are empty while the build runs.
func initReports(ctx context.Context, reader client.Reader, namespace, jobName string) (check, build string, err error) {
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		return "", "", errwrap.Wrap(err, "client.Reader.List")
	}
	for i := range pods.Items {
		var c, b string
		var built bool
		for _, cs := range pods.Items[i].Status.InitContainerStatuses {
			t := cs.State.Terminated
			if t == nil || t.ExitCode != 0 {
				continue
			}
			switch cs.Name {
			case checkContainer:
				c = t.Message
			case builderContainer:
				b, built = t.Message, true
			}
		}
		if built {
			return c, b, nil
		}
	}
	return "", "", nil
}

type diskBuildError struct{ job, msg string }

func (e *diskBuildError) Error() string {
	if e.job == "" {
		return "the disk of the image cannot be built: " + e.msg
	}
	return fmt.Sprintf("the disk Job %s failed: %s", e.job, e.msg)
}

// diskJob is the Job that makes the disk of image for the key keyID. The
// containers share the work volume. Only check and publish mount the push
// credential, and only publish mounts the signature. The build container,
// the one that reads the image, mounts neither.
func diskJob(cfg DiskBuilderConfig, diskRepo, jobName, image, keyID string) *batchv1.Job {
	job := builderJob(cfg, jobName, componentDiskBuilder, nil, nil, false)
	spec := &job.Spec.Template.Spec
	work := corev1.VolumeMount{Name: "work", MountPath: builderWorkDir}
	push := cfg.RegistrySecret != ""
	var pushMounts []corev1.VolumeMount
	var pushEnv []corev1.EnvVar
	if push {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "registry", Secret: &corev1.SecretVolumeSource{
			SecretName: cfg.RegistrySecret,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}})
		pushMounts = []corev1.VolumeMount{{Name: "registry", MountPath: "/etc/setec/registry", ReadOnly: true}}
		pushEnv = []corev1.EnvVar{{Name: "DOCKER_CONFIG", Value: "/etc/setec/registry"}}
	}
	optional := true
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "signature", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: jobName + "-sig"}, Optional: &optional,
		},
	}})
	base := spec.Containers[0]
	container := func(name string, args []string, mounts []corev1.VolumeMount, env []corev1.EnvVar) corev1.Container {
		c := *base.DeepCopy()
		c.Name, c.Args, c.Env = name, args, env
		c.VolumeMounts = append([]corev1.VolumeMount{work}, mounts...)
		return c
	}
	spec.InitContainers = []corev1.Container{
		container(checkContainer, []string{"--disk-repo", diskRepo, "--key-id", keyID, "check", image}, pushMounts, pushEnv),
		container(builderContainer, []string{"--work", builderWorkDir, "build", image}, nil, nil),
	}
	pubMounts := append(append([]corev1.VolumeMount{}, pushMounts...),
		corev1.VolumeMount{Name: "signature", MountPath: signatureDir, ReadOnly: true})
	spec.Containers = []corev1.Container{container(publishContainer, []string{
		"--disk-repo", diskRepo, "--key-id", keyID, "--work", builderWorkDir,
		"--signature-file", signatureDir + "/" + signatureKey, tempDirFlag, builderWorkDir, "publish", image,
	}, pubMounts, pushEnv)}
	return job
}

// builderJob is a Job of the setec-disk-builder image with args, one
// container. push mounts the push credential of the disk registry. No Job
// mounts the signing seed. The Job runs as a user that is not root, with no
// ServiceAccount token, and keeps its files in an emptyDir with a size limit.
func builderJob(cfg DiskBuilderConfig, jobName, component string, args []string, env []corev1.EnvVar, push bool) *batchv1.Job {
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
		Name:      jobName,
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
						// Each step writes its report to the termination message.
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
