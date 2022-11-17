/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"fmt"
	"strconv"
	"time"

	netheader "knative.dev/networking/pkg/http/header"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/ptr"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/apis/serving"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/networking"
	"knative.dev/serving/pkg/queue"
	"knative.dev/serving/pkg/reconciler/revision/config"
	"knative.dev/serving/pkg/reconciler/revision/resources/names"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

//nolint:gosec // Filepath, not hardcoded credentials
const concurrencyStateTokenVolumeMountPath = "/var/run/secrets/tokens"
const concurrencyStateTokenName = "state-token"

var (
	varLogVolume = corev1.Volume{
		Name: "knative-var-log",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}

	varLogVolumeMount = corev1.VolumeMount{
		Name:        varLogVolume.Name,
		MountPath:   "/var/log",
		SubPathExpr: "$(K_INTERNAL_POD_NAMESPACE)_$(K_INTERNAL_POD_NAME)_",
	}

	varTokenVolume = corev1.Volume{
		Name: "knative-token-volume",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						ExpirationSeconds: ptr.Int64(600),
						Path:              concurrencyStateTokenName,
						Audience:          "concurrency-state-hook"},
				}},
			},
		},
	}

	certVolumeMount = corev1.VolumeMount{
		MountPath: queue.CertDirectory,
		Name:      "server-certs",
		ReadOnly:  true,
	}

	varTokenVolumeMount = corev1.VolumeMount{
		Name:      varTokenVolume.Name,
		MountPath: concurrencyStateTokenVolumeMountPath,
	}

	// This PreStop hook is actually calling an endpoint on the queue-proxy
	// because of the way PreStop hooks are called by kubelet. We use this
	// to block the user-container from exiting before the queue-proxy is ready
	// to exit so we can guarantee that there are no more requests in flight.
	userLifecycle = &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Port:   intstr.FromInt(networking.QueueAdminPort),
				Path:   queue.RequestQueueDrainPath,
				Scheme: corev1.URISchemeHTTP,
			},
		},
	}
)

func certVolume(secret string) corev1.Volume {
	return corev1.Volume{
		Name: "server-certs",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secret,
			},
		},
	}
}

func rewriteUserProbe(p *corev1.Probe, userPort int, previousProbe *corev1.Probe) {
	if p == nil {
		return
	}
	switch {
	case p.HTTPGet != nil:
		p.HTTPGet.Port = intstr.FromInt(userPort)
		// With mTLS enabled, Istio rewrites probes, but doesn't spoof the kubelet
		// user agent, so we need to inject an extra header to be able to distinguish
		// between probes and real requests.
		p.HTTPGet.HTTPHeaders = append(p.HTTPGet.HTTPHeaders, corev1.HTTPHeader{
			Name:  netheader.KubeletProbeKey,
			Value: queue.Name,
		})

		if previousProbe != nil && previousProbe.HTTPGet != nil {
			if p.HTTPGet.Path == "" {
				p.HTTPGet.Path = previousProbe.HTTPGet.Path
			}
			if p.HTTPGet.Scheme == "" {
				p.HTTPGet.Scheme = previousProbe.HTTPGet.Scheme
			}
		}
	case p.TCPSocket != nil:
		p.TCPSocket.Port = intstr.FromInt(userPort)
	}
}

func makePodSpec(rev *v1.Revision, cfg *config.Config, previous *corev1.PodSpec) (*corev1.PodSpec, error) {
	var previousQueueContainer *corev1.Container
	var previousUserContainers []corev1.Container
	if previous != nil {
		for _, candidate := range previous.Containers {
			if candidate.Name == QueueContainerName {
				previousQueueContainer = &candidate
			} else {
				previousUserContainers = append(previousUserContainers, candidate)
			}
		}
	}
	queueContainer, err := makeQueueContainer(rev, cfg, previousQueueContainer)

	if err != nil {
		return nil, fmt.Errorf("failed to create queue-proxy container: %w", err)
	}

	var extraVolumes []corev1.Volume
	// If concurrencyStateEndpoint is enabled, add the serviceAccountToken to QP via a projected volume
	if cfg.Deployment.ConcurrencyStateEndpoint != "" {
		queueContainer.VolumeMounts = append(queueContainer.VolumeMounts, varTokenVolumeMount)
		extraVolumes = append(extraVolumes, varTokenVolume)
	}

	if cfg.Network.InternalEncryption {
		queueContainer.VolumeMounts = append(queueContainer.VolumeMounts, certVolumeMount)
		extraVolumes = append(extraVolumes, certVolume(rev.Namespace+"-"+networking.ServingCertName))
	}

	podSpec := BuildPodSpec(rev, append(BuildUserContainers(rev, previousUserContainers), *queueContainer), cfg, previous)
	podSpec.Volumes = append(podSpec.Volumes, extraVolumes...)

	if cfg.Observability.EnableVarLogCollection {
		podSpec.Volumes = append(podSpec.Volumes, varLogVolume)

		for i, container := range podSpec.Containers {
			if container.Name == QueueContainerName {
				continue
			}

			var previousContainer *corev1.Container
			if previous != nil {
				for _, candidate := range previous.Containers {
					if candidate.Name == container.Name {
						previousContainer = &candidate
						break
					}
				}
			}

			varLogMount := varLogVolumeMount.DeepCopy()
			varLogMount.SubPathExpr += container.Name
			container.VolumeMounts = append(container.VolumeMounts, *varLogMount)
			container.Env = append(container.Env, buildVarLogSubpathEnvs(previousContainer)...)

			podSpec.Containers[i] = container
		}
	}

	return podSpec, nil
}

// BuildUserContainers makes an array of containers from the Revision template.
func BuildUserContainers(rev *v1.Revision, previousContainers []corev1.Container) []corev1.Container {
	containers := make([]corev1.Container, 0, len(rev.Spec.PodSpec.Containers))
	for i := range rev.Spec.PodSpec.Containers {
		var container corev1.Container
		var previousContainer *corev1.Container
		for _, candidate := range previousContainers {
			if candidate.Name == rev.Spec.PodSpec.Containers[i].Name {
				previousContainer = &candidate
				break
			}
		}
		if len(rev.Spec.PodSpec.Containers[i].Ports) != 0 || len(rev.Spec.PodSpec.Containers) == 1 {
			container = makeServingContainer(*rev.Spec.PodSpec.Containers[i].DeepCopy(), rev, previousContainer)
		} else {
			container = makeContainer(*rev.Spec.PodSpec.Containers[i].DeepCopy(), rev, previousContainer)
		}
		// The below logic is safe because the image digests in Status.ContainerStatus will have been resolved
		// before this method is called. We check for an empty array here because the method can also be
		// called during DryRun, where ContainerStatuses will not yet have been resolved.
		if len(rev.Status.ContainerStatuses) != 0 {
			if rev.Status.ContainerStatuses[i].ImageDigest != "" {
				container.Image = rev.Status.ContainerStatuses[i].ImageDigest
			}
		}
		containers = append(containers, container)
	}
	return containers
}

func makeContainer(container corev1.Container, rev *v1.Revision, previousContainer *corev1.Container) corev1.Container {
	// Adding or removing an overwritten corev1.Container field here? Don't forget to
	// update the fieldmasks / validations in pkg/apis/serving
	container.Lifecycle = userLifecycle
	container.Env = append(container.Env, getKnativeEnvVar(rev)...)

	// Explicitly disable stdin and tty allocation
	container.Stdin = false
	container.TTY = false
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}
	if previousContainer != nil {
		container.ImagePullPolicy = previousContainer.ImagePullPolicy
		container.TerminationMessagePath = previousContainer.TerminationMessagePath
	}
	return container
}

func makeServingContainer(servingContainer corev1.Container, rev *v1.Revision, previousContainer *corev1.Container) corev1.Container {
	userPort := getUserPort(rev)
	userPortStr := strconv.Itoa(int(userPort))
	// Replacement is safe as only up to a single port is allowed on the Revision
	servingContainer.Ports = buildContainerPorts(userPort)
	servingContainer.Env = append(servingContainer.Env, buildUserPortEnv(userPortStr))
	container := makeContainer(servingContainer, rev, previousContainer)
	if container.ReadinessProbe != nil {
		if container.ReadinessProbe.HTTPGet != nil || container.ReadinessProbe.TCPSocket != nil {
			// HTTP and TCP ReadinessProbes are executed by the queue-proxy directly against the
			// user-container instead of via kubelet.
			container.ReadinessProbe = nil
		}
	}
	// If the client provides probes, we should fill in the port for them.
	var previousProbe *corev1.Probe
	if previousContainer != nil {
		previousProbe = previousContainer.LivenessProbe
	}
	rewriteUserProbe(container.LivenessProbe, int(userPort), previousProbe)
	return container
}

// BuildPodSpec creates a PodSpec from the given revision and containers.
// cfg can be passed as nil if not within revision reconciliation context.
func BuildPodSpec(rev *v1.Revision, containers []corev1.Container, cfg *config.Config, previous *corev1.PodSpec) *corev1.PodSpec {
	pod := rev.Spec.PodSpec.DeepCopy()
	pod.Containers = containers
	pod.TerminationGracePeriodSeconds = rev.Spec.TimeoutSeconds
	if cfg != nil && pod.EnableServiceLinks == nil {
		pod.EnableServiceLinks = cfg.Defaults.EnableServiceLinks
	}

	// Make sure that the DeprecatedServiceAccount is in sync with ServiceAccountName so that
	// Kubernetes does not need to perform defaulting which will caused differences
	if pod.ServiceAccountName != "" && pod.DeprecatedServiceAccount == "" {
		pod.DeprecatedServiceAccount = pod.ServiceAccountName
	}

	if previous != nil {
		pod.DNSPolicy = previous.DNSPolicy
		pod.RestartPolicy = previous.RestartPolicy
		pod.SchedulerName = previous.SchedulerName
		pod.SecurityContext = previous.SecurityContext

		// Make sure the ConfigMap and Secret volumes have a non-nil defaultMode so that
		// Kubernetes does not need to perform defaulting which will cause differences
		for i, volume := range pod.Volumes {
			// find the previous state
			var previousVolume *corev1.Volume
			for j := range previous.Volumes {
				candidate := previous.Volumes[j]
				if volume.Name == candidate.Name {
					previousVolume = &candidate
					break
				}
			}

			if previousVolume == nil {
				continue
			}

			if volume.ConfigMap != nil && volume.ConfigMap.DefaultMode == nil && previousVolume.ConfigMap != nil {
				pod.Volumes[i].ConfigMap.DefaultMode = previousVolume.ConfigMap.DefaultMode
			}

			if volume.Secret != nil && volume.Secret.DefaultMode == nil && previousVolume.Secret != nil {
				pod.Volumes[i].Secret.DefaultMode = previousVolume.Secret.DefaultMode
			}
		}
	}

	return pod
}

func getUserPort(rev *v1.Revision) int32 {
	ports := rev.Spec.GetContainer().Ports

	if len(ports) > 0 && ports[0].ContainerPort != 0 {
		return ports[0].ContainerPort
	}

	return v1.DefaultUserPort
}

func buildContainerPorts(userPort int32) []corev1.ContainerPort {
	return []corev1.ContainerPort{{
		Name:          v1.UserPortName,
		ContainerPort: userPort,
		Protocol:      corev1.ProtocolTCP,
	}}
}

func buildVarLogSubpathEnvs(previousContainer *corev1.Container) []corev1.EnvVar {
	apiVersion := ""
	if previousContainer != nil {
		for _, envVar := range previousContainer.Env {
			if envVar.Name == "K_INTERNAL_POD_NAME" && envVar.ValueFrom != nil && envVar.ValueFrom.FieldRef != nil {
				apiVersion = envVar.ValueFrom.FieldRef.APIVersion
			}
		}
	}

	return []corev1.EnvVar{{
		Name: "K_INTERNAL_POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: apiVersion,
				FieldPath:  "metadata.name",
			},
		},
	}, {
		Name: "K_INTERNAL_POD_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: apiVersion,
				FieldPath:  "metadata.namespace",
			},
		},
	}}
}

func buildUserPortEnv(userPort string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:  "PORT",
		Value: userPort,
	}
}

// MakeDeployment constructs a K8s Deployment resource from a revision.
func MakeDeployment(rev *v1.Revision, cfg *config.Config, previous *appsv1.Deployment) (*appsv1.Deployment, error) {
	var previousPodSpec *corev1.PodSpec
	if previous != nil {
		previousPodSpec = &previous.Spec.Template.Spec
	}
	podSpec, err := makePodSpec(rev, cfg, previousPodSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create PodSpec: %w", err)
	}

	replicaCount := cfg.Autoscaler.InitialScale
	_, ann, found := autoscaling.InitialScaleAnnotation.Get(rev.Annotations)
	if found {
		// Ignore errors and no error checking because already validated in webhook.
		rc, _ := strconv.ParseInt(ann, 10, 32)
		replicaCount = int32(rc)
	}

	progressDeadline := int32(cfg.Deployment.ProgressDeadline.Seconds())
	_, pdAnn, pdFound := serving.ProgressDeadlineAnnotation.Get(rev.Annotations)
	if pdFound {
		// Ignore errors and no error checking because already validated in webhook.
		pd, _ := time.ParseDuration(pdAnn)
		progressDeadline = int32(pd.Seconds())
	}

	labels := makeLabels(rev)
	anns := makeAnnotations(rev)

	// Slowly but steadily roll the deployment out, to have the least possible impact.
	maxUnavailable := intstr.FromInt(0)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.Deployment(rev),
			Namespace:       rev.Namespace,
			Labels:          labels,
			Annotations:     anns,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(rev)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.Int32(replicaCount),
			Selector:                makeSelector(rev),
			ProgressDeadlineSeconds: ptr.Int32(progressDeadline),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: anns,
				},
				Spec: *podSpec,
			},
		},
	}

	if previous != nil {
		deployment.Spec.RevisionHistoryLimit = previous.Spec.RevisionHistoryLimit
		deployment.Spec.Strategy.RollingUpdate.MaxSurge = previous.Spec.Strategy.RollingUpdate.MaxSurge
		deployment.Spec.Template.Labels = kmeta.UnionMaps(previous.Spec.Template.Labels, deployment.Spec.Template.Labels)
	}

	return deployment, nil
}
