// Copyright (c) 2018-2022 Splunk Inc. All rights reserved.

//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package enterprise

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	enterpriseApi "github.com/splunk/splunk-operator/api/v4"
	splutil "github.com/splunk/splunk-operator/pkg/splunk/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	rclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	splcommon "github.com/splunk/splunk-operator/pkg/splunk/common"
	splctrl "github.com/splunk/splunk-operator/pkg/splunk/controller"
)

// ApplyLicenseManager reconciles the state for the Splunk Enterprise license manager.
func ApplyLicenseManager(ctx context.Context, client splcommon.ControllerClient, cr *enterpriseApi.LicenseManager) (reconcile.Result, error) {

	// unless modified, reconcile for this object will be requeued after 5 seconds
	result := reconcile.Result{
		Requeue:      true,
		RequeueAfter: time.Second * 5,
	}
	reqLogger := log.FromContext(ctx)
	scopedLog := reqLogger.WithName("ApplyLicenseManager")
	eventPublisher, _ := newK8EventPublisher(client, cr)

	// validate and updates defaults for CR
	err := validateLicenseManagerSpec(ctx, client, cr)
	if err != nil {
		scopedLog.Error(err, "Failed to validate license manager spec")
		return result, err
	}

	// If needed, Migrate the app framework status
	err = checkAndMigrateAppDeployStatus(ctx, client, cr, &cr.Status.AppContext, &cr.Spec.AppFrameworkConfig, true)
	if err != nil {
		return result, err
	}

	// If the app framework is configured then do following things -
	// 1. Initialize the S3Clients based on providers
	// 2. Check the status of apps on remote storage.
	if len(cr.Spec.AppFrameworkConfig.AppSources) != 0 {
		err := initAndCheckAppInfoStatus(ctx, client, cr, &cr.Spec.AppFrameworkConfig, &cr.Status.AppContext)
		if err != nil {
			eventPublisher.Warning(ctx, "initAndCheckAppInfoStatus", fmt.Sprintf("init and check app info status failed %s", err.Error()))
			cr.Status.AppContext.IsDeploymentInProgress = false
			return result, err
		}
	}

	// updates status after function completes
	cr.Status.Phase = enterpriseApi.PhaseError

	// Update the CR Status
	defer updateCRStatus(ctx, client, cr)

	// create or update general config resources
	_, err = ApplySplunkConfig(ctx, client, cr, cr.Spec.CommonSplunkSpec, SplunkLicenseManager)
	if err != nil {
		scopedLog.Error(err, "create or update general config failed", "error", err.Error())
		eventPublisher.Warning(ctx, "ApplySplunkConfig", fmt.Sprintf("create or update general config failed with error %s", err.Error()))
		return result, err
	}

	// check if deletion has been requested
	if cr.ObjectMeta.DeletionTimestamp != nil {
		if cr.Spec.MonitoringConsoleRef.Name != "" {
			_, err = ApplyMonitoringConsoleEnvConfigMap(ctx, client, cr.GetNamespace(), cr.GetName(), cr.Spec.MonitoringConsoleRef.Name, getLicenseManagerURL(cr, &cr.Spec.CommonSplunkSpec), false)
			if err != nil {
				return result, err
			}
		}
		// If this is the last of its kind getting deleted,
		// remove the entry for this CR type from configMap or else
		// just decrement the refCount for this CR type.
		if len(cr.Spec.AppFrameworkConfig.AppSources) != 0 {
			err = UpdateOrRemoveEntryFromConfigMapLocked(ctx, client, cr, SplunkLicenseManager)
			if err != nil {
				return result, err
			}
		}

		DeleteOwnerReferencesForResources(ctx, client, cr, nil, SplunkLicenseManager)
		terminating, err := splctrl.CheckForDeletion(ctx, cr, client)

		if terminating && err != nil { // don't bother if no error, since it will just be removed immmediately after
			cr.Status.Phase = enterpriseApi.PhaseTerminating
		} else {
			result.Requeue = false
		}
		if err != nil {
			eventPublisher.Warning(ctx, "Delete", fmt.Sprintf("delete custom resource failed %s", err.Error()))
		}
		return result, err
	}

	// create or update a service
	err = splctrl.ApplyService(ctx, client, getSplunkService(ctx, cr, &cr.Spec.CommonSplunkSpec, SplunkLicenseManager, false))
	if err != nil {
		return result, err
	}

	// create or update statefulset
	statefulSet, err := getLicenseManagerStatefulSet(ctx, client, cr)
	if err != nil {
		return result, err
	}

	//make changes to respective mc configmap when changing/removing mcRef from spec
	err = validateMonitoringConsoleRef(ctx, client, statefulSet, getLicenseManagerURL(cr, &cr.Spec.CommonSplunkSpec))
	if err != nil {
		return result, err
	}

	mgr := splctrl.DefaultStatefulSetPodManager{}
	phase, err := mgr.Update(ctx, client, statefulSet, 1)
	if err != nil {
		return result, err
	}
	cr.Status.Phase = phase

	// no need to requeue if everything is ready
	if cr.Status.Phase == enterpriseApi.PhaseReady {
		//upgrade fron automated MC to MC CRD
		namespacedName := types.NamespacedName{Namespace: cr.GetNamespace(), Name: GetSplunkStatefulsetName(SplunkMonitoringConsole, cr.GetNamespace())}
		err = splctrl.DeleteReferencesToAutomatedMCIfExists(ctx, client, cr, namespacedName)
		if err != nil {
			scopedLog.Error(err, "Error in deleting automated monitoring console resource")
		}
		if cr.Spec.MonitoringConsoleRef.Name != "" {
			_, err = ApplyMonitoringConsoleEnvConfigMap(ctx, client, cr.GetNamespace(), cr.GetName(), cr.Spec.MonitoringConsoleRef.Name, getLicenseManagerURL(cr, &cr.Spec.CommonSplunkSpec), true)
			if err != nil {
				return result, err
			}
		}

		// Add a splunk operator telemetry app
		if cr.Spec.EtcVolumeStorageConfig.EphemeralStorage || !cr.Status.TelAppInstalled {
			podExecClient := splutil.GetPodExecClient(client, cr, "")
			err := addTelApp(ctx, podExecClient, numberOfLicenseMasterReplicas, cr)
			if err != nil {
				return result, err
			}

			// Mark telemetry app as installed
			cr.Status.TelAppInstalled = true
		}

		finalResult := handleAppFrameworkActivity(ctx, client, cr, &cr.Status.AppContext, &cr.Spec.AppFrameworkConfig)
		result = *finalResult
	}
	// RequeueAfter if greater than 0, tells the Controller to requeue the reconcile key after the Duration.
	// Implies that Requeue is true, there is no need to set Requeue to true at the same time as RequeueAfter.
	if !result.Requeue {
		result.RequeueAfter = 0
	}
	return result, nil
}

// getLicenseManagerStatefulSet returns a Kubernetes StatefulSet object for a Splunk Enterprise license manager.
func getLicenseManagerStatefulSet(ctx context.Context, client splcommon.ControllerClient, cr *enterpriseApi.LicenseManager) (*appsv1.StatefulSet, error) {
	ss, err := getSplunkStatefulSet(ctx, client, cr, &cr.Spec.CommonSplunkSpec, SplunkLicenseManager, 1, []corev1.EnvVar{})
	if err != nil {
		return ss, err
	}

	// Setup App framework staging volume for apps
	setupAppsStagingVolume(ctx, client, cr, &ss.Spec.Template, &cr.Spec.AppFrameworkConfig)

	return ss, err
}

// validateLicenseManagerSpec checks validity and makes default updates to a LicenseManagerSpec, and returns error if something is wrong.
func validateLicenseManagerSpec(ctx context.Context, c splcommon.ControllerClient, cr *enterpriseApi.LicenseManager) error {

	if !reflect.DeepEqual(cr.Status.AppContext.AppFrameworkConfig, cr.Spec.AppFrameworkConfig) {
		err := ValidateAppFrameworkSpec(ctx, &cr.Spec.AppFrameworkConfig, &cr.Status.AppContext, true, cr.GetObjectKind().GroupVersionKind().Kind)
		if err != nil {
			return err
		}
	}

	return validateCommonSplunkSpec(ctx, c, &cr.Spec.CommonSplunkSpec, cr)
}

// helper function to get the list of LicenseManager types in the current namespace
func getLicenseManagerList(ctx context.Context, c splcommon.ControllerClient, cr splcommon.MetaObject, listOpts []client.ListOption) (enterpriseApi.LicenseManagerList, error) {
	reqLogger := log.FromContext(ctx)
	scopedLog := reqLogger.WithName("getLicenseManagerList").WithValues("name", cr.GetName(), "namespace", cr.GetNamespace())

	objectList := enterpriseApi.LicenseManagerList{}

	err := c.List(context.TODO(), &objectList, listOpts...)
	if err != nil {
		scopedLog.Error(err, "LicenseManager types not found in namespace", "namsespace", cr.GetNamespace())
		return objectList, err
	}

	return objectList, nil
}
func getLicenseManagerCurrentImage(ctx context.Context, c splcommon.ControllerClient, cr *enterpriseApi.LicenseManager) (string, error) {

	reqLogger := log.FromContext(ctx)
	scopedLog := reqLogger.WithName("getLicenseManagerCurrentImage").WithValues("name", cr.GetName(), "namespace", cr.GetNamespace())
	eventPublisher, _ := newK8EventPublisher(c, cr)

	namespacedName := types.NamespacedName{
		Namespace: cr.GetNamespace(),
		Name:      GetSplunkStatefulsetName(SplunkLicenseManager, cr.GetName()),
	}
	statefulSet := &appsv1.StatefulSet{}
	err := c.Get(ctx, namespacedName, statefulSet)
	if err != nil {
		eventPublisher.Warning(ctx, "getLicenseManagerCurrentImage", fmt.Sprintf("Could not get Stateful Set. Reason %v", err))
		scopedLog.Error(err, "StatefulSet types not found in namespace", "namsespace", cr.GetNamespace())
		return "", err
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(statefulSet.Spec.Selector)
	if err != nil {
		eventPublisher.Warning(ctx, "getLicenseManagerCurrentImage", fmt.Sprintf("Could not get labels. Reason %v", err))
		scopedLog.Error(err, "Unable to get labels")
		return "", err
	}

	// get a list of all pods in the namespace with matching labels as the statefulset
	statefulsetPods := &corev1.PodList{}
	opts := []rclient.ListOption{
		rclient.InNamespace(cr.GetNamespace()),
		rclient.MatchingLabelsSelector{Selector: labelSelector},
	}

	err = c.List(ctx, statefulsetPods, opts...)
	if err != nil {
		eventPublisher.Warning(ctx, "getLicenseManagerCurrentImage", fmt.Sprintf("Could not get Pod list. Reason %v", err))
		scopedLog.Error(err, "Pods types not found in namespace", "namsespace", cr.GetNamespace())
		return "", err
	}

	// find the container with the phrase 'splunk' in it
	for _, v := range statefulsetPods.Items {
		for _, container := range v.Status.ContainerStatuses {
			if strings.Contains(container.Name, "splunk") {
				image := container.Image
				return image, nil
			}

		}
	}

	return "", nil
}
