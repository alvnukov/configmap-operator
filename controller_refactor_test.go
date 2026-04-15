package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"go.uber.org/zap"
)

func TestParseConfig_StrictUnknownField(t *testing.T) {
	_, err := ParseConfig([]byte(`
deployments:
  - namespace: ns
    name: dep
    containers:
      - name: app
        unknownField: value
        configMaps: []
`))
	if err == nil {
		t.Fatalf("expected ParseConfig to fail on unknown field")
	}
	if !strings.Contains(err.Error(), "unknownField") {
		t.Fatalf("expected strict parser error to mention unknownField, got: %v", err)
	}
}

func TestValidateConfig_DetectsDuplicateDeployment(t *testing.T) {
	cfg := &DeploymentConfig{
		Deployments: []DeploymentInfo{
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app",
						ConfigMaps: []ConfigMapMount{{
							Name: "cm",
							Data: []ConfigMapData{{Key: "k", Value: "v", MountPath: "/etc/k"}},
						}},
					},
				},
			},
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app2",
						ConfigMaps: []ConfigMapMount{{
							Name: "cm2",
							Data: []ConfigMapData{{Key: "k", Value: "v", MountPath: "/etc/k"}},
						}},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for duplicate deployment")
	}
	if !strings.Contains(err.Error(), "duplicate deployment") {
		t.Fatalf("expected duplicate deployment error, got: %v", err)
	}
}

func TestValidateConfig_DetectsConflictingConfigMapDataAcrossContainers(t *testing.T) {
	cfg := &DeploymentConfig{
		Deployments: []DeploymentInfo{
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app-a",
						ConfigMaps: []ConfigMapMount{{
							Name: "shared-cm",
							Data: []ConfigMapData{{Key: "k", Value: "value-a", MountPath: "/etc/a"}},
						}},
					},
					{
						Name: "app-b",
						ConfigMaps: []ConfigMapMount{{
							Name: "shared-cm",
							Data: []ConfigMapData{{Key: "k", Value: "value-b", MountPath: "/etc/b"}},
						}},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for conflicting ConfigMap data")
	}
	if !strings.Contains(err.Error(), "conflicting data definitions") {
		t.Fatalf("expected conflicting data error, got: %v", err)
	}
}

func TestValidateConfig_DetectsDuplicateMountTargetAcrossConfigMaps(t *testing.T) {
	cfg := &DeploymentConfig{
		Deployments: []DeploymentInfo{
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app",
						ConfigMaps: []ConfigMapMount{
							{
								Name: "cm-a",
								Data: []ConfigMapData{{Key: "a", Value: "1", MountPath: "/etc/shared", SubPath: "app.conf"}},
							},
							{
								Name: "cm-b",
								Data: []ConfigMapData{{Key: "b", Value: "2", MountPath: "/etc/shared", SubPath: "app.conf"}},
							},
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for duplicate mount target")
	}
	if !strings.Contains(err.Error(), "used by multiple configMaps") {
		t.Fatalf("expected duplicate mount target error, got: %v", err)
	}
}

func TestDesiredDeploymentHash_ChangesOnConfigMutation(t *testing.T) {
	deployment := &DeploymentInfo{
		Namespace: "ns",
		Name:      "dep",
		Containers: []ContainerConfig{
			{
				Name: "app",
				ConfigMaps: []ConfigMapMount{{
					Name: "cm",
					Data: []ConfigMapData{{Key: "k", Value: "value-a", MountPath: "/etc/file", SubPath: "file"}},
				}},
			},
		},
	}

	hashA := desiredDeploymentHash(deployment)
	deployment.Containers[0].ConfigMaps[0].Data[0].Value = "value-b"
	hashB := desiredDeploymentHash(deployment)

	if hashA == hashB {
		t.Fatalf("expected deployment hash to change after config mutation")
	}
}

func TestDesiredDeploymentHash_IsOrderInsensitive(t *testing.T) {
	deploymentA := &DeploymentInfo{
		Namespace: "ns",
		Name:      "dep",
		Containers: []ContainerConfig{
			{
				Name: "b",
				ConfigMaps: []ConfigMapMount{{
					Name: "cm-b",
					Data: []ConfigMapData{
						{Key: "kb", Value: "vb", MountPath: "/etc/b", SubPath: "b"},
					},
				}},
			},
			{
				Name: "a",
				ConfigMaps: []ConfigMapMount{{
					Name: "cm-a",
					Data: []ConfigMapData{
						{Key: "ka2", Value: "va2", MountPath: "/etc/a2", SubPath: "a2"},
						{Key: "ka1", Value: "va1", MountPath: "/etc/a1", SubPath: "a1"},
					},
				}},
			},
		},
	}

	deploymentB := &DeploymentInfo{
		Namespace: "ns",
		Name:      "dep",
		Containers: []ContainerConfig{
			{
				Name: "a",
				ConfigMaps: []ConfigMapMount{{
					Name: "cm-a",
					Data: []ConfigMapData{
						{Key: "ka1", Value: "va1", MountPath: "/etc/a1", SubPath: "a1"},
						{Key: "ka2", Value: "va2", MountPath: "/etc/a2", SubPath: "a2"},
					},
				}},
			},
			{
				Name: "b",
				ConfigMaps: []ConfigMapMount{{
					Name: "cm-b",
					Data: []ConfigMapData{
						{Key: "kb", Value: "vb", MountPath: "/etc/b", SubPath: "b"},
					},
				}},
			},
		},
	}

	hashA := desiredDeploymentHash(deploymentA)
	hashB := desiredDeploymentHash(deploymentB)
	if hashA != hashB {
		t.Fatalf("expected equal hashes for semantically equal configs, got %q vs %q", hashA, hashB)
	}
}

func TestReconcileDeployment_UpdatesConfigMapAndRolloutAnnotation(t *testing.T) {
	cfg := &DeploymentConfig{
		Deployments: []DeploymentInfo{
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app",
						ConfigMaps: []ConfigMapMount{{
							Name: "cm",
							Data: []ConfigMapData{{
								Key:       "config.yaml",
								Value:     "new-value",
								SubPath:   "config.yaml",
								MountPath: "/etc/app/config.yaml",
							}},
						}},
					},
				},
			},
		},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "dep",
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
		},
	}

	existingConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "cm",
		},
		Data: map[string]string{
			"config.yaml": "old-value",
		},
	}

	clientset := fake.NewSimpleClientset(deployment, existingConfigMap)
	controller := NewControllerWithConfig(clientset, zap.NewNop(), cfg)

	deploymentInfo, ok := cfg.FindDeployment("ns", "dep")
	if !ok {
		t.Fatalf("test setup failure: deployment not found in config")
	}

	if err := controller.reconcileDeployment(context.Background(), deploymentInfo); err != nil {
		t.Fatalf("reconcileDeployment returned error: %v", err)
	}

	cmAfter, err := clientset.CoreV1().ConfigMaps("ns").Get(context.Background(), "cm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap after reconcile: %v", err)
	}
	if cmAfter.Data["config.yaml"] != "new-value" {
		t.Fatalf("expected ConfigMap data to be updated, got %q", cmAfter.Data["config.yaml"])
	}
	if cmAfter.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("expected managed-by label to be set")
	}

	depAfter, err := clientset.AppsV1().Deployments("ns").Get(context.Background(), "dep", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Deployment after reconcile: %v", err)
	}

	hashAnnotation := depAfter.Spec.Template.Annotations[podTemplateConfigHashAnnotation]
	if hashAnnotation == "" {
		t.Fatalf("expected config hash annotation to be set")
	}

	rolloutAnnotation := depAfter.Spec.Template.Annotations[podTemplateRolloutAnnotation]
	if rolloutAnnotation == "" {
		t.Fatalf("expected rollout annotation to be set when ConfigMap changed")
	}

	if len(depAfter.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("expected one volume in deployment template, got %d", len(depAfter.Spec.Template.Spec.Volumes))
	}

	if len(depAfter.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("expected one volume mount in deployment template, got %d", len(depAfter.Spec.Template.Spec.Containers[0].VolumeMounts))
	}

	if err := controller.reconcileDeployment(context.Background(), deploymentInfo); err != nil {
		t.Fatalf("second reconcileDeployment returned error: %v", err)
	}

	depAfterSecondRun, err := clientset.AppsV1().Deployments("ns").Get(context.Background(), "dep", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Deployment after second reconcile: %v", err)
	}

	if depAfterSecondRun.Spec.Template.Annotations[podTemplateRolloutAnnotation] != rolloutAnnotation {
		t.Fatalf("expected rollout annotation to stay stable when ConfigMap is unchanged")
	}
}
