// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
	// with the push credentials of the disk repository. Only the check and
	// publish containers mount it.
	RegistrySecret string
	// PullSecret, when set, is a kubernetes.io/dockerconfigjson Secret with
	// a credential to read the launcher image from a private registry. Only
	// the build container and the image verify container mount it. It is a
	// different Secret from RegistrySecret, so the container that reads the
	// image never holds the push credential.
	PullSecret string
	// PullProviders name the cloud credential providers that the build and
	// image verify containers use to read a private registry, such as "ecr"
	// (internal/diskbuilder/registryauth). The provider uses the identity of
	// the Job: the web identity token of PullAWSRoleARN, the Pod Identity
	// agent, or the role of the node.
	PullProviders []string
	// PullAWSRoleARN, when set, is an IAM role that the build and image
	// verify containers assume with a web identity token. The operator
	// projects a token of the Job ServiceAccount for the audience
	// sts.amazonaws.com into those containers only. The role must grant pull
	// rights and no push right: the Job ServiceAccount is the same for each
	// container, and the check and publish containers hold no token.
	PullAWSRoleARN string
	// PushProviders name the cloud credential providers that the check and
	// publish containers use to push to the disk repository, such as "ecr".
	// Only those two containers get the provider and its identity. The build
	// container and the image verify container never do.
	PushProviders []string
	// PushAWSRoleARN is the IAM role that the check and publish containers
	// assume with a web identity token. The operator projects a second token
	// of the Job ServiceAccount into those two containers, for the audience
	// PushAWSAudience, which differs from the audience of the pull token. The
	// trust policy of the push role requires that audience, so the token of
	// the build container, which has the audience of the pull token, cannot
	// assume the push role.
	PushAWSRoleARN string
	// PushAWSAudience is the audience of the push token. The IAM OpenID
	// Connect provider of the cluster lists it as a client ID.
	PushAWSAudience string
	// Instance is the name of the release that runs the operator. It goes on
	// each Job, Pod and ConfigMap that the operator makes as the label
	// app.kubernetes.io/instance, so that a check of the namespace finds the
	// objects of the release. Empty leaves the label out.
	Instance string
	// Placement says where the Job Pods may run. The zero value keeps the
	// default scheduling, with the fixed architecture selector.
	Placement DiskJobPlacement
	// ImagePullSecrets are Secrets of the operator namespace that the Job
	// Pods use to pull the image of the disk builder from a private registry.
	ImagePullSecrets []string
	// ServiceAccount is the ServiceAccount of the Job Pods. Empty means the
	// default of the namespace. The Pod has no API token. An identity that
	// the cloud binds to a ServiceAccount (IAM roles for service accounts,
	// EKS Pod Identity) needs a named one.
	ServiceAccount string
	// Reader reads the Jobs straight from the API server. The cache of the
	// manager would need list and watch on Jobs in every namespace; the
	// operator holds get and create in its own namespace only.
	Reader client.Reader
	// Now is the clock of the retry backoff. Nil means time.Now.
	Now func() time.Time
}

// objectLabels are the labels of each object that the operator makes in its
// own namespace for the disk builder: the Jobs, their Pods and the signature
// ConfigMaps. The component label is the one that the chart policy selects a
// Pod by. The others say who made the object and for which release, so that
// a check that expects each object to have a manager finds one. The label
// app.kubernetes.io/name is not here: a Service or a policy of the chart
// selects by name and instance, and no Job Pod may match it.
func (cfg DiskBuilderConfig) objectLabels(component string) map[string]string {
	labels := map[string]string{
		componentLabel:                 component,
		"app.kubernetes.io/managed-by": "setec-operator",
		"app.kubernetes.io/part-of":    "setec",
	}
	if cfg.Instance != "" {
		labels["app.kubernetes.io/instance"] = cfg.Instance
	}
	return labels
}

// DiskJobPlacement is where the Pods of the disk Jobs may run. The chart
// values launcher.diskBuilder.nodeSelector, tolerations, affinity and
// priorityClassName fill it. A cluster that taints its amd64 nodes, or limits
// the nodes where a Job may run, needs it. The key kubernetes.io/arch of the
// selector is fixed: the disk builder image is single-arch linux/amd64, and a
// value cannot override it.
type DiskJobPlacement struct {
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations       []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity          *corev1.Affinity    `json:"affinity,omitempty"`
	PriorityClassName string              `json:"priorityClassName,omitempty"`
}

// ParseDiskJobPlacement reads the JSON of the flag --disk-job-placement. An
// empty string is the zero placement. A field that the type does not know is an
// error, so a misspelled key stops the operator at start.
func ParseDiskJobPlacement(raw string) (DiskJobPlacement, error) {
	var p DiskJobPlacement
	if raw == "" {
		return p, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return DiskJobPlacement{}, fmt.Errorf("the disk Job placement: %w", err)
	}
	return p, nil
}

// nodeSelector is the node selector of a Job Pod: the keys of the install,
// and the fixed architecture key on top.
func (p DiskJobPlacement) nodeSelector() map[string]string {
	out := make(map[string]string, len(p.NodeSelector)+1)
	maps.Copy(out, p.NodeSelector)
	out["kubernetes.io/arch"] = "amd64"
	return out
}

func (cfg DiskBuilderConfig) now() time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}

// Reasons of a launcher Sandbox that waits for or lost its disk.
const (
	// ReasonDiskBuilding is a Sandbox that waits for a build to end.
	ReasonDiskBuilding = "DiskBuilding"
	// ReasonDiskBuildRetrying is a Sandbox that waits for the next build
	// after a build failed. The Warning Event names the attempt, the cause
	// and the time of the next attempt.
	ReasonDiskBuildRetrying = "DiskBuildRetrying"
	// ReasonDiskBuildFailed is a Sandbox whose image failed every attempt.
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
	// componentLabel is the label that the chart policy selects a pod by.
	componentLabel = "app.kubernetes.io/component"
	// The volumes of a disk Job.
	workVolume     = "work"
	registryVolume = "registry"
	registryDir    = "/etc/setec/registry"
	// The pull credential of the build and image verify containers. These
	// are names of volumes and paths, not credentials.
	pullVolume = "pull-registry"            //nolint:gosec // a volume name
	pullDir    = "/etc/setec/pull-registry" //nolint:gosec // a path
	// The web identity token of the build and image verify containers.
	awsTokenVolume   = "aws-pull-token"             //nolint:gosec // a volume name
	awsTokenDir      = "/var/run/secrets/setec/aws" //nolint:gosec // a path
	awsTokenAudience = "sts.amazonaws.com"          //nolint:gosec // the audience of a token
	awsTokenSeconds  = 3600
	// The web identity token of the check and publish containers. Its audience
	// is PushAWSAudience, so the pull token cannot assume the push role.
	awsPushTokenVolume = "aws-push-token"                  //nolint:gosec // a volume name
	awsPushTokenDir    = "/var/run/secrets/setec/aws-push" //nolint:gosec // a path
	// builderWorkDir is the emptyDir of a disk builder Job.
	builderWorkDir   = "/work"
	diskBuildRequeue = 5 * time.Second
	// diskJobTTLSeconds is how long a finished Job of an attempt that is not
	// the last one stays. The next attempt starts before it ends: the longest
	// backoff is shorter.
	diskJobTTLSeconds = 600
	// diskJobLastTTLSeconds is how long the Job of the last attempt stays. A
	// failed one is the record that no new build starts for an image that
	// fails each time: the next Sandbox of the image starts a new series
	// only when this Job has expired.
	diskJobLastTTLSeconds = 3600
	// attemptAnnotation names the attempt of a disk Job.
	attemptAnnotation = "setec.zeroroot.ai/disk-build-attempt"
)

// diskBuildBackoff is the wait after each failed attempt, before the next
// one starts. A series has one attempt more than the list has entries, so a
// build that cannot work runs four times in about eight minutes and then
// stops for an hour (diskJobLastTTLSeconds). Each wait is shorter than
// diskJobTTLSeconds, so the Job that records the failure is still there.
var diskBuildBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// diskBuildAttempts is the number of attempts of one series.
func diskBuildAttempts() int { return len(diskBuildBackoff) + 1 }

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

// diskAttemptJobName names the Job of attempt n (from 1). The first attempt
// has the name of the digest and the key. Each later attempt has its own Job,
// so the Pods, the signature ConfigMap and the failure record of an attempt
// never mix with another, and the operator needs no delete right.
func diskAttemptJobName(base string, n int) string {
	if n <= 1 {
		return base
	}
	return fmt.Sprintf("%s-a%d", base, n)
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
	baseName, err := diskJobName(image, keyID)
	if err != nil {
		return "", false, err
	}
	// The newest Job of the series is the one that counts. A Job of an
	// earlier attempt may have expired already.
	var job *batchv1.Job
	attempt := 0
	for n := diskBuildAttempts(); n >= 1; n-- {
		got := &batchv1.Job{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: diskAttemptJobName(baseName, n)}, got)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", false, errwrap.Wrap(err, "client.Reader.Get")
		}
		job, attempt = got, n
		break
	}
	if job == nil {
		return keyID, false, startDiskAttempt(ctx, c, cfg, diskRepo, baseName, image, keyID, 1)
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return keyID, true, nil
		case batchv1.JobFailed:
			if err := nextDiskAttempt(ctx, c, cfg, diskRepo, baseName, image, keyID, attempt, cond); err != nil {
				return "", false, err
			}
			return keyID, false, nil
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Not an end: JobComplete or JobFailed follows.
		}
	}
	if err := signBuiltDisk(ctx, c, reader, cfg, job, image, key); err != nil {
		return "", false, err
	}
	return keyID, false, nil
}

// startDiskAttempt creates the Job of attempt n.
func startDiskAttempt(
	ctx context.Context, c client.Client, cfg DiskBuilderConfig, diskRepo, baseName, image, keyID string, n int,
) error {
	job := diskJob(cfg, diskRepo, diskAttemptJobName(baseName, n), image, keyID)
	job.Annotations = map[string]string{attemptAnnotation: strconv.Itoa(n)}
	if n >= diskBuildAttempts() {
		ttl := int32(diskJobLastTTLSeconds)
		job.Spec.TTLSecondsAfterFinished = &ttl
	}
	if err := c.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the disk Job %s: %w", job.Name, err)
	}
	return nil
}

// nextDiskAttempt decides what follows a failed Job of attempt n. While the
// backoff of the attempt runs, it returns a diskRetryError. After it, it
// starts attempt n+1 and returns nil, so the caller waits for that build.
// After the last attempt it returns a diskBuildError: the image fails each
// time, and the failed Job stays for an hour before a new series can start.
func nextDiskAttempt(
	ctx context.Context, c client.Client, cfg DiskBuilderConfig, diskRepo, baseName, image, keyID string,
	n int, failed batchv1.JobCondition,
) error {
	jobName := diskAttemptJobName(baseName, n)
	if n >= diskBuildAttempts() {
		return &diskBuildError{job: jobName, msg: fmt.Sprintf(
			"the build failed %d times, the last time with %q. No new build starts for this image for about %d minutes",
			n, failed.Message, diskJobLastTTLSeconds/60)}
	}
	now := cfg.now()
	if due := failed.LastTransitionTime.Add(diskBuildBackoff[n-1]); now.Before(due) {
		return &diskRetryError{job: jobName, attempt: n, msg: failed.Message, after: due.Sub(now)}
	}
	return startDiskAttempt(ctx, c, cfg, diskRepo, baseName, image, keyID, n+1)
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
		Labels: cfg.objectLabels(componentDiskBuilder),
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

// diskRetryError is a build that failed with attempts left. It is not an
// end: the next attempt starts after the backoff.
type diskRetryError struct {
	job     string
	attempt int
	msg     string
	after   time.Duration
}

func (e *diskRetryError) Error() string {
	return fmt.Sprintf("the disk Job %s failed (attempt %d of %d) with %q. The next attempt starts in %s",
		e.job, e.attempt, diskBuildAttempts(), e.msg, e.after.Round(time.Second))
}

type diskBuildError struct{ job, msg string }

func (e *diskBuildError) Error() string {
	if e.job == "" {
		return "the disk of the image cannot be built: " + e.msg
	}
	return fmt.Sprintf("the disk Job %s failed: %s", e.job, e.msg)
}

// grantPull gives container c the means to read a private registry: the pull
// Secret, the web identity token, and the providers that ask the cloud. The
// container is the one that reads an image, so it gets no more than that. It
// never gets the push Secret or the signing seed: those have their own
// mounts, in the other containers.
func (cfg DiskBuilderConfig) grantPull(spec *corev1.PodSpec, c *corev1.Container) {
	args := make([]string, 0, 2*len(cfg.PullProviders)+len(c.Args))
	for _, p := range cfg.PullProviders {
		args = append(args, "--registry-auth", p)
	}
	c.Args = append(args, c.Args...)
	if cfg.PullSecret != "" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: pullVolume, Secret: &corev1.SecretVolumeSource{
			SecretName: cfg.PullSecret,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: pullVolume, MountPath: pullDir, ReadOnly: true})
		c.Env = append(c.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: pullDir})
	}
	if cfg.PullAWSRoleARN != "" {
		seconds := int64(awsTokenSeconds)
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: awsTokenVolume, Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: awsTokenAudience, ExpirationSeconds: &seconds, Path: "token",
			}}},
		}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: awsTokenVolume, MountPath: awsTokenDir, ReadOnly: true})
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "AWS_ROLE_ARN", Value: cfg.PullAWSRoleARN},
			corev1.EnvVar{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: awsTokenDir + "/token"},
			corev1.EnvVar{Name: "AWS_ROLE_SESSION_NAME", Value: "setec-disk-builder"})
	}
}

// ValidatePushIdentity checks the flags of the push identity at startup, so a
// mistake stops the operator and does not wait for the first disk build. A
// provider must be one that the builder knows. The provider ecr needs a web
// identity role, because that is the one way that keeps the push identity out
// of the build container: the identity that the cluster gives to a Pod (EKS
// Pod Identity, the role of the node) reaches each container of the Job Pod.
// The audience of the push token must differ from the audience of the pull
// token, because the two tokens belong to the same ServiceAccount and the
// audience is the one claim that tells them apart.
func ValidatePushIdentity(providers []string, roleARN, audience, serviceAccount string) error {
	for _, p := range providers {
		if !slices.Contains(diskbuilder.PushProviders, p) {
			return fmt.Errorf("--disk-push-provider %q is not known (known: %s)", p, strings.Join(diskbuilder.PushProviders, ", "))
		}
	}
	hasECR := slices.Contains(providers, "ecr")
	switch {
	case roleARN != "" && !hasECR:
		return errors.New("--disk-push-aws-role-arn needs --disk-push-provider=ecr: the role is used by that provider")
	case hasECR && roleARN == "":
		return errors.New("--disk-push-provider=ecr needs --disk-push-aws-role-arn. The identity of the Pod or of the node " +
			"reaches the build container too, so only a web identity token, which only the check and publish containers hold, keeps the push identity out of it")
	case roleARN != "" && !strings.HasPrefix(roleARN, "arn:"):
		return fmt.Errorf("--disk-push-aws-role-arn must be an IAM role ARN, got %q", roleARN)
	case roleARN != "" && serviceAccount == "":
		return errors.New("--disk-push-aws-role-arn needs --disk-service-account: the token that the role trusts belongs to it")
	case roleARN != "" && (audience == "" || audience == awsTokenAudience):
		return fmt.Errorf("--disk-push-aws-audience must name an audience other than %s, the audience of the pull token. "+
			"With the same audience the build container could assume the push role", awsTokenAudience)
	}
	return nil
}

// grantPush gives container c the means to push to the disk repository with
// an identity of the cloud: the providers, and the web identity token of the
// push role. Only the check and publish containers call it. The container
// that reads an image gets none of it (grantPull). The token volume is added
// once, and each of the two containers mounts it.
func (cfg DiskBuilderConfig) grantPush(spec *corev1.PodSpec, c *corev1.Container) {
	args := make([]string, 0, 2*len(cfg.PushProviders)+len(c.Args))
	for _, p := range cfg.PushProviders {
		args = append(args, "--push-auth", p)
	}
	c.Args = append(args, c.Args...)
	if cfg.PushAWSRoleARN == "" {
		return
	}
	if !slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == awsPushTokenVolume }) {
		seconds := int64(awsTokenSeconds)
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: awsPushTokenVolume, Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: cfg.PushAWSAudience, ExpirationSeconds: &seconds, Path: "token",
			}}},
		}})
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: awsPushTokenVolume, MountPath: awsPushTokenDir, ReadOnly: true})
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "AWS_ROLE_ARN", Value: cfg.PushAWSRoleARN},
		corev1.EnvVar{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: awsPushTokenDir + "/token"},
		corev1.EnvVar{Name: "AWS_ROLE_SESSION_NAME", Value: "setec-disk-publisher"})
}

// diskJob is the Job that makes the disk of image for the key keyID. The
// containers share the work volume. Only check and publish mount the push
// credential, and only publish mounts the signature. The build container,
// the one that reads the image, mounts neither. It mounts the pull
// credential, when the install has one (grantPull).
func diskJob(cfg DiskBuilderConfig, diskRepo, jobName, image, keyID string) *batchv1.Job {
	job := builderJob(cfg, jobName, componentDiskBuilder, nil, nil)
	spec := &job.Spec.Template.Spec
	work := corev1.VolumeMount{Name: workVolume, MountPath: builderWorkDir}
	var pushMounts []corev1.VolumeMount
	var pushEnv []corev1.EnvVar
	if cfg.RegistrySecret != "" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: registryVolume, Secret: &corev1.SecretVolumeSource{
			SecretName: cfg.RegistrySecret,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}})
		pushMounts = []corev1.VolumeMount{{Name: registryVolume, MountPath: registryDir, ReadOnly: true}}
		pushEnv = []corev1.EnvVar{{Name: "DOCKER_CONFIG", Value: registryDir}}
	}
	optional := true
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "signature", ConfigMap: &corev1.ConfigMapVolumeSource{
		Name: jobName + "-sig", Optional: &optional,
	}})
	base := spec.Containers[0]
	container := func(name string, args []string, mounts []corev1.VolumeMount, env []corev1.EnvVar) corev1.Container {
		c := *base.DeepCopy()
		c.Name, c.Args, c.Env = name, args, env
		c.VolumeMounts = append([]corev1.VolumeMount{work}, mounts...)
		return c
	}
	build := container(builderContainer, []string{"--work", builderWorkDir, "build", image}, nil, nil)
	cfg.grantPull(spec, &build)
	check := container(checkContainer, []string{"--disk-repo", diskRepo, "--key-id", keyID, "check", image}, pushMounts, pushEnv)
	cfg.grantPush(spec, &check)
	spec.InitContainers = []corev1.Container{check, build}
	pubMounts := append(append([]corev1.VolumeMount{}, pushMounts...),
		corev1.VolumeMount{Name: "signature", MountPath: signatureDir, ReadOnly: true})
	publish := container(publishContainer, []string{
		"--disk-repo", diskRepo, "--key-id", keyID, "--work", builderWorkDir,
		"--signature-file", signatureDir + "/" + signatureKey, tempDirFlag, builderWorkDir, "publish", image,
	}, pubMounts, pushEnv)
	cfg.grantPush(spec, &publish)
	spec.Containers = []corev1.Container{publish}
	return job
}

// pullSecretRefs turns Secret names into the references of a Pod. It returns
// nil for no name, so a Pod with no pull Secret has no field.
func pullSecretRefs(names []string) []corev1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return refs
}

// builderJob is a Job of the setec-disk-builder image with args, one
// container and the work volume. It mounts no Secret: diskJob adds the push
// credential to the containers that need it. The Job runs as a user that is
// not root, with no ServiceAccount token, and keeps its files in an
// emptyDir with a size limit.
func builderJob(cfg DiskBuilderConfig, jobName, component string, args []string, env []corev1.EnvVar) *batchv1.Job {
	ttl := int32(diskJobTTLSeconds)
	backoff := int32(2)
	work := resource.MustParse("20Gi")
	return &batchv1.Job{
		Name:      jobName,
		Namespace: cfg.Namespace,
		Labels:    cfg.objectLabels(component),
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Labels: cfg.objectLabels(component),
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           cfg.ServiceAccount,
					ImagePullSecrets:             pullSecretRefs(cfg.ImagePullSecrets),
					AutomountServiceAccountToken: new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					NodeSelector:      cfg.Placement.nodeSelector(),
					Tolerations:       cfg.Placement.Tolerations,
					Affinity:          cfg.Placement.Affinity,
					PriorityClassName: cfg.Placement.PriorityClassName,
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
						VolumeMounts: []corev1.VolumeMount{{Name: workVolume, MountPath: builderWorkDir}},
					}},
					Volumes: []corev1.Volume{{Name: workVolume, EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &work}}},
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
		if retry, ok := err.(*diskRetryError); ok { //nolint:errorlint // a local type, never wrapped
			if r.Recorder != nil {
				r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, ReasonDiskBuildRetrying, actionFinalizeSandbox, "%s", retry.Error())
			}
			if err := r.patchPendingStatus(ctx, sb, ReasonDiskBuildRetrying); err != nil {
				return "", false, ctrl.Result{}, err
			}
			return "", false, ctrl.Result{RequeueAfter: max(retry.after, time.Second)}, nil
		}
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
