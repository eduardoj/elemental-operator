/*
Copyright © 2022 - 2023 SUSE LLC

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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	errorutils "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
)

type SeedImageReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=elemental.cattle.io,resources=seedimages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=elemental.cattle.io,resources=seedimages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineregistrations,verbs=get;watch;list
// TODO: restrict access to pods to the required namespace only
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get

// TODO: extend SetupWithManager with "Watches" and "WithEventFilter"
func (r *SeedImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&elementalv1.SeedImage{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func (r *SeedImageReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	seedImg := &elementalv1.SeedImage{}
	if err := r.Get(ctx, req.NamespacedName, seedImg); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(5).Info("Object was not found, not an error")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get seedimage object: %w", err)
	}

	patchBase := client.MergeFrom(seedImg.DeepCopy())

	// Collect errors as an aggregate to return together after all patches have been performed.
	var errs []error

	result, err := r.reconcile(ctx, seedImg)
	if err != nil {
		errs = append(errs, fmt.Errorf("error reconciling seedimage object: %w", err))
	}

	seedImgStatusCopy := seedImg.Status.DeepCopy() // Patch call will erase the status

	if err := r.Patch(ctx, seedImg, patchBase); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("failed to patch status for seedimage object: %w", err))
	}

	seedImg.Status = *seedImgStatusCopy

	if err := r.Status().Patch(ctx, seedImg, patchBase); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("failed to patch status for seedimage object: %w", err))
	}

	if len(errs) > 0 {
		return ctrl.Result{}, errorutils.NewAggregate(errs)
	}

	return result, nil
}

func (r *SeedImageReconciler) reconcile(ctx context.Context, seedImg *elementalv1.SeedImage) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling seedimage object")

	// Init the Ready condition as we want it to be the first one displayed
	meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
		Type:   elementalv1.ReadyCondition,
		Reason: elementalv1.ResourcesSuccessfullyCreatedReason,
		Status: metav1.ConditionUnknown,
	})

	if err := r.reconcileBuildImagePod(ctx, seedImg); err != nil {
		meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.PodCreationFailureReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to create pod: %w", err)
	}

	if err := r.createBuildImageService(ctx, seedImg); err != nil {
		meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.ServiceCreationFailureReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to create service: %w", err)
	}

	meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
		Type:    elementalv1.ReadyCondition,
		Reason:  elementalv1.ResourcesSuccessfullyCreatedReason,
		Status:  metav1.ConditionTrue,
		Message: "resources created successfully",
	})

	return ctrl.Result{}, nil
}

func (r *SeedImageReconciler) reconcileBuildImagePod(ctx context.Context, seedImg *elementalv1.SeedImage) error {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling Pod resources")

	mRegistration := &elementalv1.MachineRegistration{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      seedImg.Spec.MachineRegistrationRef.Name,
		Namespace: seedImg.Spec.MachineRegistrationRef.Namespace,
	}, mRegistration); err != nil {
		return err
	}

	podName := seedImg.Name
	podNamespace := seedImg.Namespace
	podBaseImg := seedImg.Spec.BaseImage
	podRegURL := mRegistration.Status.RegistrationURL

	foundPod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, foundPod)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil {
		logger.V(5).Info("Pod already there")

		// ensure the pod was created by us
		podIsOwned := false
		for _, owner := range foundPod.GetOwnerReferences() {
			if owner.UID == seedImg.UID {
				podIsOwned = true
				break
			}
		}
		if !podIsOwned {
			return fmt.Errorf("pod already exists and was not created by this controller")
		}

		// pod is already there and with the right configuration
		if foundPod.Annotations["elemental.cattle.io/base-image"] == podBaseImg &&
			foundPod.Annotations["elemental.cattle.io/registration-url"] == podRegURL {

			switch foundPod.Status.Phase {
			case corev1.PodPending:
				meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
					Type:    elementalv1.SeedImageConditionReady,
					Status:  metav1.ConditionFalse,
					Reason:  elementalv1.SeedImageBuildOngoingReason,
					Message: "seed image build ongoing",
				})
				return nil
			case corev1.PodRunning:
				rancherURL, err := r.getRancherServerAddress(ctx)
				if err != nil {
					errMsg := fmt.Errorf("failed to get Rancher Server Address: %w", err)
					meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
						Type:    elementalv1.SeedImageConditionReady,
						Status:  metav1.ConditionFalse,
						Reason:  elementalv1.SeedImageExposeFailureReason,
						Message: errMsg.Error(),
					})
					return errMsg
				}
				svc := &corev1.Service{}
				if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, svc); err != nil {
					errMsg := fmt.Errorf("failed to get associated service: %w", err)
					meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
						Type:    elementalv1.SeedImageConditionReady,
						Status:  metav1.ConditionFalse,
						Reason:  elementalv1.SeedImageExposeFailureReason,
						Message: errMsg.Error(),
					})
					return errMsg
				}
				seedImg.Status.DownloadURL = fmt.Sprintf("http://%s:%d/elemental.iso", rancherURL, svc.Spec.Ports[0].NodePort)
				meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
					Type:    elementalv1.SeedImageConditionReady,
					Status:  metav1.ConditionTrue,
					Reason:  elementalv1.SeedImageBuildSuccessReason,
					Message: "seed image iso available",
				})
				return nil
			case corev1.PodFailed:
				meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
					Type:    elementalv1.SeedImageConditionReady,
					Status:  metav1.ConditionFalse,
					Reason:  elementalv1.SeedImageBuildFailureReason,
					Message: "pod failed",
				})
				return nil
			default:
				meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
					Type:   elementalv1.SeedImageConditionReady,
					Status: metav1.ConditionUnknown,
				})
			}
		}

		// Pod is not up-to-date: delete and recreate it
		if err := r.Delete(ctx, foundPod); err != nil {
			return fmt.Errorf("failed to delete old pod: %w", err)
		}
	}

	// apierrors.IsNotFound(err) OR we just deleted the old Pod

	logger.V(5).Info("Creating pod")

	pod := fillBuildImagePod(podName, podNamespace, podBaseImg, podRegURL)
	if err := controllerutil.SetControllerReference(seedImg, pod, r.Scheme()); err != nil {
		meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
			Type:    elementalv1.SeedImageConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.SeedImageBuildNotStartedReason,
			Message: err.Error(),
		})
		return err
	}

	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
			Type:    elementalv1.SeedImageConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.SeedImageBuildNotStartedReason,
			Message: err.Error(),
		})
		return err
	}

	meta.SetStatusCondition(&seedImg.Status.Conditions, metav1.Condition{
		Type:    elementalv1.SeedImageConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  elementalv1.SeedImageBuildOngoingReason,
		Message: "seed image build started",
	})
	return nil
}

func (r *SeedImageReconciler) getRancherServerAddress(ctx context.Context) (string, error) {
	logger := ctrl.LoggerFrom(ctx)

	setting := &managementv3.Setting{}
	if err := r.Get(ctx, types.NamespacedName{Name: "server-url"}, setting); err != nil {
		return "", fmt.Errorf("failed to get server url setting: %w", err)
	}

	if setting.Value == "" {
		err := fmt.Errorf("server-url is not set")
		logger.Error(err, "can't get server-url")
		return "", err
	}

	return strings.TrimPrefix(setting.Value, "https://"), nil
}

func fillBuildImagePod(name, namespace, baseImg, regURL string) *corev1.Pod {
	const buildImage = "registry.opensuse.org/isv/rancher/elemental/stable/teal53/15.4/rancher/elemental-builder-image/5.3:latest"
	const serveImage = "registry.opensuse.org/opensuse/nginx:latest"
	const volLim = 4 * 1024 * 1024 * 1024 // 4 GiB
	const volRes = 2 * 1024 * 1024 * 1024 // 2 GiB

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": name},
			Annotations: map[string]string{
				"elemental.cattle.io/base-image":       baseImg,
				"elemental.cattle.io/registration-url": regURL,
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name:  "build",
					Image: buildImage,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceEphemeralStorage: *resource.NewQuantity(volLim, resource.BinarySI),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceEphemeralStorage: *resource.NewQuantity(volRes, resource.BinarySI),
						},
					},
					Command: []string{"/bin/bash", "-c"},
					Args: []string{
						fmt.Sprintf("%s && %s && %s",
							fmt.Sprintf("curl -Lo base.img %s", baseImg),
							fmt.Sprintf("curl -ko reg.yaml %s", regURL),
							"xorriso -indev base.img -outdev /iso/elemental.iso -map reg.yaml /reg.yaml -boot_image any replay"),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "iso-storage",
							MountPath: "/iso",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "serve",
					Image: serveImage,
					Ports: []corev1.ContainerPort{
						{
							Name:          "http",
							ContainerPort: 80,
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "iso-storage",
							MountPath: "/srv/www/htdocs",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "iso-storage",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resource.NewQuantity(volLim, resource.BinarySI),
						},
					},
				},
			},
		},
	}
	return pod
}

func (r *SeedImageReconciler) createBuildImageService(ctx context.Context, seedImg *elementalv1.SeedImage) error {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling Service resources")

	logger.V(5).Info("Creating service")

	service := fillBuildImageService(seedImg.Name, seedImg.Namespace)

	if err := controllerutil.SetControllerReference(seedImg, service, r.Scheme()); err != nil {
		return err
	}

	if err := r.Create(ctx, service); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func fillBuildImageService(name, namespace string) *corev1.Service {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": name},
			Ports: []corev1.ServicePort{
				{
					Protocol: corev1.ProtocolTCP,
					Port:     80,
				},
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}

	return service
}