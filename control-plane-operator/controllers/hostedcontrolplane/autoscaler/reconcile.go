package autoscaler

import (
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/kas"
	"github.com/openshift/hypershift/support/config"
	"github.com/openshift/hypershift/support/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sutilspointer "k8s.io/utils/pointer"
)

func ReconcileAutoscalerDeployment(deployment *appsv1.Deployment, hcp *hyperv1.HostedControlPlane, sa *corev1.ServiceAccount, kubeConfigSecret *corev1.Secret, options hyperv1.ClusterAutoscaling, clusterAutoscalerImage, availabilityProberImage string, setDefaultSecurityContext bool) error {
	config.OwnerRefFrom(hcp).ApplyTo(deployment)
	args := []string{
		"--cloud-provider=clusterapi",
		"--node-group-auto-discovery=clusterapi:namespace=$(MY_NAMESPACE)",
		"--kubeconfig=/mnt/kubeconfig/target-kubeconfig",
		"--clusterapi-cloud-config-authoritative",
		// TODO (alberto): Is this a fair assumption?
		// There's currently pods with local storage e.g grafana and image-registry.
		// Without this option after after a scaling out operation and an “unfortunate” reschedule
		// we might end up locked with three nodes.
		"--skip-nodes-with-local-storage=false",
		"--alsologtostderr",
		fmt.Sprintf("--leader-elect-lease-duration=%s", config.RecommendedLeaseDuration),
		fmt.Sprintf("--leader-elect-retry-period=%s", config.RecommendedRetryPeriod),
		fmt.Sprintf("--leader-elect-renew-deadline=%s", config.RecommendedRenewDeadline),
		"--v=4",
	}

	// TODO if the options for the cluster autoscaler continues to grow, we should take inspiration
	// from the cluster-autoscaler-operator and create some utility functions for these assignments.
	if options.MaxNodesTotal != nil {
		arg := fmt.Sprintf("%s=%d", "--max-nodes-total", *options.MaxNodesTotal)
		args = append(args, arg)
	}

	if options.MaxPodGracePeriod != nil {
		arg := fmt.Sprintf("%s=%d", "--max-graceful-termination-sec", *options.MaxPodGracePeriod)
		args = append(args, arg)
	}

	if options.MaxNodeProvisionTime != "" {
		arg := fmt.Sprintf("%s=%s", "--max-node-provision-time", options.MaxNodeProvisionTime)
		args = append(args, arg)
	}

	if options.PodPriorityThreshold != nil {
		arg := fmt.Sprintf("%s=%d", "--expendable-pods-priority-cutoff", *options.PodPriorityThreshold)
		args = append(args, arg)
	}

	labels := map[string]string{
		"app":                         "cluster-autoscaler",
		hyperv1.ControlPlaneComponent: "cluster-autoscaler",
	}
	// The selector needs to be invariant for the lifecycle of the project as it's an immutable field,
	// otherwise changing would prevent an upgrade from happening.
	selector := map[string]string{
		"app": "cluster-autoscaler",
	}

	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: selector,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: corev1.PodSpec{
				ServiceAccountName:            sa.Name,
				TerminationGracePeriodSeconds: k8sutilspointer.Int64Ptr(10),
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "target-kubeconfig",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: kubeConfigSecret.Name,
								Items: []corev1.KeyToPath{
									{
										// TODO: should the key be published on status?
										Key:  "value",
										Path: "target-kubeconfig",
									},
								},
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "cluster-autoscaler",
						Image:           clusterAutoscalerImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "target-kubeconfig",
								MountPath: "/mnt/kubeconfig",
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("35Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						Command: []string{"/usr/bin/cluster-autoscaler"},
						Args:    args,
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/health-check",
									Port:   intstr.FromInt(8085),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							InitialDelaySeconds: 60,
							PeriodSeconds:       60,
							SuccessThreshold:    1,
							FailureThreshold:    5,
							TimeoutSeconds:      5,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/health-check",
									Port:   intstr.FromInt(8085),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       60,
							SuccessThreshold:    1,
							FailureThreshold:    3,
							TimeoutSeconds:      5,
						},
						Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
					},
				},
			},
		},
	}

	util.AvailabilityProber(kas.InClusterKASReadyURL(deployment.Namespace, util.APIPort(hcp)), availabilityProberImage, &deployment.Spec.Template.Spec)

	deploymentConfig := config.DeploymentConfig{
		Replicas: 1,
		Scheduling: config.Scheduling{
			PriorityClass: config.DefaultPriorityClass,
		},
		SetDefaultSecurityContext: setDefaultSecurityContext,
	}

	deploymentConfig.SetReleaseImageAnnotation(hcp.Spec.ReleaseImage)
	deploymentConfig.SetColocation(hcp)
	deploymentConfig.SetRestartAnnotation(hcp.ObjectMeta)
	deploymentConfig.SetControlPlaneIsolation(hcp)
	deploymentConfig.ApplyTo(deployment)

	return nil
}

func ReconcileAutoscalerRole(role *rbacv1.Role, owner config.OwnerRef) error {
	owner.ApplyTo(role)
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"cluster.x-k8s.io"},
			Resources: []string{
				"machinedeployments",
				"machinedeployments/scale",
				"machines",
				"machinesets",
				"machinesets/scale",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

func ReconcileAutoscalerRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount, owner config.OwnerRef) error {
	owner.ApplyTo(binding)
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}

	return nil
}