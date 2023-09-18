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

package revision

import (
	"context"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	cachingclientset "knative.dev/caching/pkg/client/clientset/versioned"
	clientset "knative.dev/serving/pkg/client/clientset/versioned"
	revisionreconciler "knative.dev/serving/pkg/client/injection/reconciler/serving/v1/revision"

	cachinglisters "knative.dev/caching/pkg/client/listers/caching/v1alpha1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	palisters "knative.dev/serving/pkg/client/listers/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/reconciler/revision/config"
)

type resolver interface {
	Resolve(*zap.SugaredLogger, *v1.Revision, k8schain.Options, sets.String, time.Duration) ([]v1.ContainerStatus, []v1.ContainerStatus, error)
	Clear(types.NamespacedName)
	Forget(types.NamespacedName)
}

// Reconciler implements controller.Reconciler for Revision resources.
type Reconciler struct {
	kubeclient    kubernetes.Interface
	client        clientset.Interface
	cachingclient cachingclientset.Interface

	// lister indexes properties about Revision
	podAutoscalerLister palisters.PodAutoscalerLister
	imageLister         cachinglisters.ImageLister
	deploymentLister    appsv1listers.DeploymentLister

	resolver resolver
}

// Check that our Reconciler implements the necessary interfaces.
var (
	_ revisionreconciler.Interface      = (*Reconciler)(nil)
	_ pkgreconciler.OnDeletionInterface = (*Reconciler)(nil)
)

func (c *Reconciler) reconcileDigest(ctx context.Context, rev *v1.Revision) (bool, error) {
	totalNumOfContainers := len(rev.Spec.Containers) + len(rev.Spec.InitContainers)

	// The image digest has already been resolved.
	// No need to check for init containers feature flag here because rev.Spec has been validated already
	if len(rev.Status.ContainerStatuses)+len(rev.Status.InitContainerStatuses) == totalNumOfContainers {
		c.resolver.Clear(types.NamespacedName{Namespace: rev.Namespace, Name: rev.Name})
		return true, nil
	}

	imagePullSecrets := make([]string, 0, len(rev.Spec.ImagePullSecrets))
	for _, s := range rev.Spec.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, s.Name)
	}
	cfgs := config.FromContext(ctx)
	opt := k8schain.Options{
		Namespace:          rev.Namespace,
		ServiceAccountName: rev.Spec.ServiceAccountName,
		ImagePullSecrets:   imagePullSecrets,
	}

	logger := logging.FromContext(ctx)
	initContainerStatuses, statuses, err := c.resolver.Resolve(logger, rev, opt, cfgs.Deployment.RegistriesSkippingTagResolving, cfgs.Deployment.DigestResolutionTimeout)
	if err != nil {
		// Clear the resolver so we can retry the digest resolution rather than
		// being stuck with this error.
		c.resolver.Clear(types.NamespacedName{Namespace: rev.Namespace, Name: rev.Name})
		rev.Status.MarkContainerHealthyFalse(v1.ReasonContainerMissing, err.Error())
		return true, err
	}

	if len(statuses) > 0 || len(initContainerStatuses) > 0 {
		rev.Status.ContainerStatuses = statuses
		rev.Status.InitContainerStatuses = initContainerStatuses
		return true, nil
	}

	// No digest yet, wait for re-enqueue when resolution is done.
	return false, nil
}

// ReconcileKind implements Interface.ReconcileKind.
func (c *Reconciler) ReconcileKind(ctx context.Context, rev *v1.Revision) pkgreconciler.Event {
	ctx, cancel := context.WithTimeout(ctx, pkgreconciler.DefaultTimeout)
	defer cancel()

	readyBeforeReconcile := rev.IsReady()
	c.updateRevisionLoggingURL(ctx, rev)

	// IBM Patch: Exit if 'pause' or 'fail' label is there.
	// We use a label instead of an annotation so we can quickly find it.
	// The format of the label should be:
	//    codeengine.cloud.ibm.com/pause-REASON: DATA
	//    codeengine.cloud.ibm.com/fail-REASON: DATA
	// where REASON should be the high level reason for the pause (e.g. "build"
	// to mean we're waiting for a build to complete). DATA is an instance
	// specific piece of metadata about what we're waiting for (e.g.
	// a buildrun name or UID).
	// Watch for the need to wait for multiple instances of a resource.
	// For example, watching for multiple service bindings. In this case
	// the REASON will probably need service binding instance specific
	// data to avoid a name conflict. Technically, the code here does not
	// care what REASON and DATA are, it's really up to the code that will
	// set/look for these labels, so that code can decide these details.
	failLabels := []string{}
	hasPauseLabels := false
	for name := range rev.Labels {
		if strings.HasPrefix(name, "codeengine.cloud.ibm.com/fail-") {
			failLabels = append(failLabels, name)
		}

		if strings.HasPrefix(name, "codeengine.cloud.ibm.com/pause-") {
			hasPauseLabels = true
		}
	}

	if len(failLabels) > 0 {
		var messageBuilder strings.Builder
		for i, label := range failLabels {
			messageBuilder.WriteString(rev.Labels[label])
			if i < len(failLabels)-1 {
				messageBuilder.WriteString("; ")
			}
		}

		rev.Status.MarkResourcesAvailableFalse("Codeengine-Failure", messageBuilder.String())
		return nil
	}

	if hasPauseLabels {
		rev.Status.MarkResourcesAvailableUnknown("Paused", "")
		return nil
	}

	reconciled, err := c.reconcileDigest(ctx, rev)
	if err != nil {
		return err
	}
	if !reconciled {
		// Digest not resolved yet, wait for background resolution to re-enqueue the revision.
		rev.Status.MarkResourcesAvailableUnknown(v1.ReasonResolvingDigests, "")
		return nil
	}

	// Spew is an expensive operation so guard the computation on the debug level
	// being enabled.
	// Some things, like PA reachability, etc are computed based on various labels/annotations
	// of revision. So it is useful to provide this information for debugging.
	logger := logging.FromContext(ctx).Desugar()
	if logger.Core().Enabled(zapcore.DebugLevel) {
		logger.Debug("Revision meta: " + spew.Sdump(rev.ObjectMeta))
	}

	// Deploy certificate when internal-encryption is enabled.
	if config.FromContext(ctx).Network.InternalEncryption {
		if err := c.reconcileSecret(ctx, rev); err != nil {
			return err
		}
	}

	for _, phase := range []func(context.Context, *v1.Revision) error{
		c.reconcileDeployment,
		c.reconcileImageCache,
		c.reconcilePA,
	} {
		if err := phase(ctx, rev); err != nil {
			return err
		}
	}
	readyAfterReconcile := rev.Status.GetCondition(v1.RevisionConditionReady).IsTrue()
	if !readyBeforeReconcile && readyAfterReconcile {
		logger.Info("Revision became ready")
		controller.GetEventRecorder(ctx).Event(
			rev, corev1.EventTypeNormal, "RevisionReady",
			"Revision becomes ready upon all resources being ready")
	} else if readyBeforeReconcile && !readyAfterReconcile {
		logger.Info("Revision stopped being ready")
	}

	return nil
}

func (c *Reconciler) updateRevisionLoggingURL(ctx context.Context, rev *v1.Revision) {
	config := config.FromContext(ctx)
	if config.Observability.LoggingURLTemplate == "" {
		rev.Status.LogURL = ""
		return
	}

	rev.Status.LogURL = strings.ReplaceAll(
		config.Observability.LoggingURLTemplate,
		"${REVISION_UID}", string(rev.UID))
}

// ObserveDeletion implements OnDeletionInterface.ObserveDeletion.
func (c *Reconciler) ObserveDeletion(ctx context.Context, key types.NamespacedName) error {
	c.resolver.Forget(key)
	return nil
}
