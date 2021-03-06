/*

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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/keikoproj/instance-manager/api/v1alpha1"
	"github.com/keikoproj/instance-manager/controllers/common"
	awsprovider "github.com/keikoproj/instance-manager/controllers/providers/aws"
	kubeprovider "github.com/keikoproj/instance-manager/controllers/providers/kubernetes"
	"github.com/keikoproj/instance-manager/controllers/provisioners"
	"github.com/keikoproj/instance-manager/controllers/provisioners/eks"
	"github.com/keikoproj/instance-manager/controllers/provisioners/eksfargate"
	"github.com/keikoproj/instance-manager/controllers/provisioners/eksmanaged"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// InstanceGroupReconciler reconciles an InstanceGroup object
type InstanceGroupReconciler struct {
	client.Client
	SpotRecommendationTime float64
	ConfigNamespace        string
	NodeRelabel            bool
	Log                    logr.Logger
	MaxParallel            int
	Auth                   *InstanceGroupAuthenticator
	ConfigMap              *corev1.ConfigMap
}

type InstanceGroupAuthenticator struct {
	Aws        awsprovider.AwsWorker
	Kubernetes kubeprovider.KubernetesClientSet
}

func (r *InstanceGroupReconciler) Finalize(instanceGroup *v1alpha1.InstanceGroup, finalizerName string) {
	// Resource is being deleted
	meta := &instanceGroup.ObjectMeta
	deletionTimestamp := meta.GetDeletionTimestamp()
	if !deletionTimestamp.IsZero() {
		// And state is "Deleted"
		if instanceGroup.GetState() == v1alpha1.ReconcileDeleted {
			// Unset Finalizer if present
			if common.ContainsString(meta.GetFinalizers(), finalizerName) {
				meta.SetFinalizers(common.RemoveString(instanceGroup.ObjectMeta.Finalizers, finalizerName))
				if err := r.Update(context.Background(), instanceGroup); err != nil {
					r.Log.Error(err, "failed to update custom resource")
				}
			}
		}
	}
}

func (r *InstanceGroupReconciler) SetFinalizer(instanceGroup *v1alpha1.InstanceGroup, finalizerName string) {
	// Resource is not being deleted
	if instanceGroup.ObjectMeta.DeletionTimestamp.IsZero() {
		// And does not contain finalizer
		if !common.ContainsString(instanceGroup.ObjectMeta.Finalizers, finalizerName) {
			// Set Finalizer
			instanceGroup.ObjectMeta.Finalizers = append(instanceGroup.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(context.Background(), instanceGroup); err != nil {
				r.Log.Error(err, "failed to update custom resource")
			}
		}
	}
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=list;patch;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;create;update;patch;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=instancemgr.keikoproj.io,resources=instancegroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=instancemgr.keikoproj.io,resources=instancegroups/status,verbs=get;update;patch

func (r *InstanceGroupReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("instancegroup", req.NamespacedName)

	instanceGroup := &v1alpha1.InstanceGroup{}
	err := r.Get(context.Background(), req.NamespacedName, instanceGroup)
	if err != nil {
		if kerrors.IsNotFound(err) {
			r.Log.Info("instancegroup not found", "instancegroup", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		r.Log.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}

	// set/unset finalizer
	finalizerName := fmt.Sprintf("finalizers.%v.instancegroups.keikoproj.io", instanceGroup.Spec.Provisioner)
	r.SetFinalizer(instanceGroup, finalizerName)
	defer r.Finalize(instanceGroup, finalizerName)

	var defaultConfig *provisioners.DefaultConfiguration
	if defaultConfig, err = provisioners.UnmarshalConfiguration(r.ConfigMap); err != nil {
		r.Log.Error(err, "failed to unmarshal configuration", "instancegroup", instanceGroup.NamespacedName())
		return ctrl.Result{}, err
	}

	configuredInstanceGroup := &v1alpha1.InstanceGroup{}
	instanceGroup.DeepCopyInto(configuredInstanceGroup)

	if configuredInstanceGroup, err = provisioners.SetConfigurationDefaults(configuredInstanceGroup, defaultConfig); err != nil {
		r.Log.Error(err, "failed to set configuration defaults", "instancegroup", instanceGroup.NamespacedName())
		return ctrl.Result{}, err
	}

	input := provisioners.ProvisionerInput{
		AwsWorker:     r.Auth.Aws,
		Kubernetes:    r.Auth.Kubernetes,
		InstanceGroup: configuredInstanceGroup,
		Configuration: r.ConfigMap,
		Log:           r.Log,
	}

	provisionerKind := strings.ToLower(configuredInstanceGroup.Spec.Provisioner)

	if !common.ContainsEqualFold(v1alpha1.Provisioners, provisionerKind) {
		return ctrl.Result{}, errors.Errorf("provisioner '%v' does not exist", provisionerKind)
	}

	r.Log.Info("reconcile event started", "instancegroup", req.NamespacedName, "provisioner", provisionerKind)

	// defer updates for the instanceGroup CR
	defer r.UpdateStatus(configuredInstanceGroup)

	var isRetryable bool
	if strings.EqualFold(provisionerKind, eks.ProvisionerName) {
		ctx := eks.New(input)

		if err = configuredInstanceGroup.Validate(); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		if err = HandleReconcileRequest(ctx); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		isRetryable = eks.IsRetryable(configuredInstanceGroup)
	}

	if strings.EqualFold(provisionerKind, eksmanaged.ProvisionerName) {
		ctx := eksmanaged.New(input)

		if err = configuredInstanceGroup.Validate(); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		if err = HandleReconcileRequest(ctx); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		isRetryable = eksmanaged.IsRetryable(configuredInstanceGroup)
	}

	if strings.EqualFold(provisionerKind, eksfargate.ProvisionerName) {
		ctx := eksfargate.New(input)

		if err = configuredInstanceGroup.Validate(); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		if err = HandleReconcileRequest(ctx); err != nil {
			ctx.SetState(v1alpha1.ReconcileErr)
			return ctrl.Result{}, errors.Wrapf(err, "provisioner %v reconcile failed", provisionerKind)
		}

		isRetryable = eksfargate.IsRetryable(configuredInstanceGroup)
	}

	if isRetryable {
		r.Log.Info("reconcile event ended with requeue", "instancegroup", req.NamespacedName, "provisioner", provisionerKind)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *InstanceGroupReconciler) UpdateStatus(ig *v1alpha1.InstanceGroup) {
	r.Log.Info("updating resource status", "instancegroup", ig.NamespacedName())
	if err := r.Status().Update(context.Background(), ig); err != nil {
		r.Log.Info("failed to update status", "error", err, "instancegroup", ig.NamespacedName())
	}
}
