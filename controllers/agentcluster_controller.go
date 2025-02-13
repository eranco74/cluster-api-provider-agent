/*
Copyright 2021.

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

	"github.com/openshift/hive/apis/hive/v1/agent"

	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AgentClusterReconciler reconciles a AgentCluster object
type AgentClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logrus.FieldLogger
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterimagesets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.hive.openshift.io,resources=agentclusterinstalls,verbs=get;list;watch;create;update;patch;delete

func (r *AgentClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent_cluster":           req.Name,
			"agent_cluster_namespace": req.Namespace,
		})

	defer func() {
		log.Info("AgentCluster Reconcile ended")
	}()

	agentCluster := &capiproviderv1alpha1.AgentCluster{}
	if err := r.Get(ctx, req.NamespacedName, agentCluster); err != nil {
		log.WithError(err).Errorf("Failed to get agentCluster %s", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If the agentCluster has no reference to a ClusterDeployment, create one
	if agentCluster.Status.ClusterDeploymentRef.Name == "" {
		return r.createClusterDeployment(ctx, log, agentCluster)
	}
	clusterDeployment := &hivev1.ClusterDeployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: agentCluster.Status.ClusterDeploymentRef.Namespace, Name: agentCluster.Status.ClusterDeploymentRef.Name}, clusterDeployment)
	if err != nil {
		log.WithError(err).Error("Failed to get ClusterDeployment")
		return ctrl.Result{Requeue: true}, err
	}
	if clusterDeployment.Spec.ClusterInstallRef == nil {
		return r.SetAgentClusterInstallRef(ctx, log, clusterDeployment)
	}

	result, err := r.updateAgentClusterInstall(ctx, log, agentCluster, clusterDeployment)
	if err != nil {
		return result, err
	}
	if !agentCluster.Spec.ControlPlaneEndpoint.IsValid() {
		log.Info("Waiting for agentCluster controlPlaneEndpoint")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, nil
	}
	// If the agentCluster has references a ClusterDeployment, sync from its status
	return r.updateClusterStatus(ctx, log, agentCluster)
}

func (r *AgentClusterReconciler) updateAgentClusterInstall(ctx context.Context, log logrus.FieldLogger, agentCluster *capiproviderv1alpha1.AgentCluster, clusterDeployment *hivev1.ClusterDeployment) (ctrl.Result, error) {
	// Make sure the agentClusterInstall imageSetRef match the agentCluster.Spec.releaseImage
	agentClusterInstall := &hiveext.AgentClusterInstall{}
	err := r.Get(ctx, types.NamespacedName{Namespace: agentCluster.Status.ClusterDeploymentRef.Namespace, Name: clusterDeployment.Spec.ClusterInstallRef.Name}, agentClusterInstall)
	if err != nil {
		log.WithError(err).Error("Failed to get AgentClusterInstall")
		return ctrl.Result{Requeue: true}, err
	}
	updateACI := false

	if agentClusterInstall.Spec.ProvisionRequirements.ControlPlaneAgents < 1 {
		agentClusterInstall.Spec.ProvisionRequirements.ControlPlaneAgents = 3
		updateACI = true
	}

	if agentClusterInstall.Spec.ImageSetRef.Name == "" {
		agentClusterInstall.Spec.ImageSetRef = hivev1.ClusterImageSetReference{Name: agentCluster.Name}
		updateACI = true

	}

	if agentClusterInstall.Spec.IgnitionEndpoint == nil && agentCluster.Spec.IgnitionEndpoint != nil {
		log.Info("Updating ignition endpoint")
		url := agentCluster.Spec.IgnitionEndpoint.Url
		agentClusterInstall.Spec.IgnitionEndpoint = &hiveext.IgnitionEndpoint{
			// Currently assume something like https://1.2.3.4:555/ignition, otherwise this will fail
			// TODO: Replace with something more robust
			Url: url[0:strings.LastIndex(url, "/")],
		}
		if agentCluster.Spec.IgnitionEndpoint.CaCertificateReference != nil {
			agentClusterInstall.Spec.IgnitionEndpoint.CaCertificateReference = &hiveext.CaCertificateReference{
				Namespace: agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Namespace,
				Name:      agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Name,
			}
		}
		updateACI = true
	}
	if updateACI {
		if err = r.Client.Update(ctx, agentClusterInstall); err != nil {
			log.WithError(err).Error("Failed to update agentClusterInstall imageSetRef")
			return ctrl.Result{Requeue: true}, err
		}
	}
	clusterImageSet := &hivev1.ClusterImageSet{}
	err = r.Get(ctx, types.NamespacedName{Namespace: "", Name: agentClusterInstall.Spec.ImageSetRef.Name}, clusterImageSet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.createImageSet(ctx, log, agentClusterInstall, agentCluster)
		}
		log.WithError(err).Error("Failed to get ClusterImageSet")
		return ctrl.Result{Requeue: true}, err
	}

	if clusterImageSet.Spec.ReleaseImage != agentCluster.Spec.ReleaseImage {
		err = fmt.Errorf("clusterImageSet ReleaseImage (%s) doens't match agentCluster ReleaseImage (%s)",
			clusterImageSet.Spec.ReleaseImage, agentCluster.Spec.ReleaseImage)
		log.Error(err)
		return ctrl.Result{Requeue: true}, err
	}
	return ctrl.Result{}, nil
}

func (r *AgentClusterReconciler) createImageSet(ctx context.Context, log logrus.FieldLogger, agentClusterInstall *hiveext.AgentClusterInstall, agentCluster *capiproviderv1alpha1.AgentCluster) (ctrl.Result, error) {
	log.Info("Creating ClusterImageSet")
	clusterImageSet := &hivev1.ClusterImageSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterImageSet",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentCluster.Name,
			Namespace: "",
		},
		Spec: hivev1.ClusterImageSetSpec{ReleaseImage: agentCluster.Spec.ReleaseImage},
	}
	if err := r.Client.Create(ctx, clusterImageSet); err != nil {
		log.WithError(err).Error("Failed to create ClusterImageSet")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *AgentClusterReconciler) createClusterDeployment(ctx context.Context, log logrus.FieldLogger, agentCluster *capiproviderv1alpha1.AgentCluster) (ctrl.Result, error) {
	log.Info("Creating clusterDeployment")
	clusterDeployment := &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentCluster.Name,
			Namespace: agentCluster.Namespace,
		},
		Spec: hivev1.ClusterDeploymentSpec{
			Installed:     true,
			BaseDomain:    agentCluster.Spec.BaseDomain,
			ClusterName:   agentCluster.Spec.ClusterName,
			PullSecretRef: agentCluster.Spec.PullSecretRef,
			Platform: hivev1.Platform{
				AgentBareMetal: &agent.BareMetalPlatform{},
			},
		},
	}
	agentCluster.Status.ClusterDeploymentRef.Name = clusterDeployment.Name
	agentCluster.Status.ClusterDeploymentRef.Namespace = clusterDeployment.Namespace
	if err := r.Client.Create(ctx, clusterDeployment); err != nil {
		log.WithError(err).Error("Failed to create ClusterDeployment")
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.Client.Status().Update(ctx, agentCluster); err != nil {
		log.WithError(err).Error("Failed to update status")
		return ctrl.Result{Requeue: true}, err
	}
	return ctrl.Result{}, nil
}

func (r *AgentClusterReconciler) SetAgentClusterInstallRef(ctx context.Context, log logrus.FieldLogger, clusterDeployment *hivev1.ClusterDeployment) (ctrl.Result, error) {
	log.Info("Setting AgentClusterInstall")
	agentClusterInstall := &hiveext.AgentClusterInstall{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: clusterDeployment.Namespace, Name: clusterDeployment.Name}, agentClusterInstall); err != nil {
		if apierrors.IsNotFound(err) {
			err = r.createAgentClusterInstall(ctx, log, clusterDeployment)
			if err != nil {
				log.WithError(err).Error("failed to create AgentClusterInstall")
				return ctrl.Result{Requeue: true}, err
			}
		} else {
			log.WithError(err).Error("Failed to get AgentClusterInstall")
			return ctrl.Result{Requeue: true}, err
		}
	}
	clusterDeployment.Spec.ClusterInstallRef = &hivev1.ClusterInstallLocalReference{
		Kind:    "AgentClusterInstall",
		Group:   hiveext.Group,
		Version: hiveext.Version,
		Name:    clusterDeployment.Name,
	}

	if err := r.Update(ctx, clusterDeployment); err != nil {
		log.WithError(err).Error("Failed to update ClusterInstallRef on clusterDeployment")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err

	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *AgentClusterReconciler) createAgentClusterInstall(ctx context.Context, log logrus.FieldLogger, clusterDeployment *hivev1.ClusterDeployment) error {
	log.Infof("Creating AgentClusterInstall for clusterDeployment: %s %s", clusterDeployment.Namespace, clusterDeployment.Name)
	agentClusterInstall := &hiveext.AgentClusterInstall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterDeployment.Name,
			Namespace: clusterDeployment.Namespace,
		},
		Spec: hiveext.AgentClusterInstallSpec{
			ClusterDeploymentRef: v1.LocalObjectReference{Name: clusterDeployment.Name},
		},
	}
	return r.Client.Create(ctx, agentClusterInstall)
}

func (r *AgentClusterReconciler) updateClusterStatus(ctx context.Context, log logrus.FieldLogger, agentCluster *capiproviderv1alpha1.AgentCluster) (ctrl.Result, error) {
	log.Infof("Updating agentCluster status according to %s", agentCluster.Status.ClusterDeploymentRef.Name)
	// Once the cluster have clusterDeploymentRef and ClusterInstallRef we should set the status to Ready
	agentCluster.Status.Ready = true
	if err := r.Status().Update(ctx, agentCluster); err != nil {
		log.WithError(err).Error("Failed to set ready status")
		return ctrl.Result{Requeue: true}, nil

	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capiproviderv1alpha1.AgentCluster{}).
		Complete(r)
}
