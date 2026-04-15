package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"go.uber.org/zap"
)

func TestHasConfigMapVolumeMount_MatchesNamePathAndSubPath(t *testing.T) {
	volumeMounts := []corev1.VolumeMount{
		{Name: "cm", MountPath: "/etc/a", SubPath: "a.conf"},
	}

	if !hasConfigMapVolumeMount(volumeMounts, "cm", "/etc/a", "a.conf") {
		t.Fatalf("expected exact mount to be found")
	}

	if hasConfigMapVolumeMount(volumeMounts, "cm", "/etc/b", "a.conf") {
		t.Fatalf("expected mount with different mountPath to be treated as missing")
	}
}

func TestAddConfigMapIfMissing_AddsDistinctMountsAndVolumeItems(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	controller := &Controller{
		clientset: clientset,
		logger:    zap.NewNop(),
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dep",
			Namespace: "ns",
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app"},
					},
				},
			},
		},
	}

	config := &DeploymentConfig{
		Deployments: []DeploymentInfo{
			{
				Namespace: "ns",
				Name:      "dep",
				Containers: []ContainerConfig{
					{
						Name: "app",
						ConfigMaps: []ConfigMapMount{
							{
								Name: "cm1",
								Data: []ConfigMapData{
									{Key: "a", Value: "1", MountPath: "/etc/a", SubPath: "a.conf"},
									{Key: "b", Value: "2", MountPath: "/etc/b"},
								},
							},
						},
					},
				},
			},
		},
	}

	if !controller.AddConfigMapIfMissing(deployment, config) {
		t.Fatalf("expected first call to report changes")
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(container.VolumeMounts))
	}

	mountsByPath := make(map[string]corev1.VolumeMount, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		mountsByPath[mount.MountPath] = mount
	}

	if mount, ok := mountsByPath["/etc/a"]; !ok || mount.Name != "cm1" || mount.SubPath != "a.conf" {
		t.Fatalf("expected mount /etc/a with subPath a.conf, got %#v", mount)
	}

	if mount, ok := mountsByPath["/etc/b"]; !ok || mount.Name != "cm1" || mount.SubPath != "" {
		t.Fatalf("expected mount /etc/b without subPath, got %#v", mount)
	}

	if len(deployment.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(deployment.Spec.Template.Spec.Volumes))
	}

	volume := deployment.Spec.Template.Spec.Volumes[0]
	if volume.ConfigMap == nil {
		t.Fatalf("expected ConfigMap volume source")
	}

	items := make(map[string]string, len(volume.ConfigMap.Items))
	for _, item := range volume.ConfigMap.Items {
		items[item.Key] = item.Path
	}

	if items["a"] != "a.conf" {
		t.Fatalf("expected item a path to be a.conf, got %q", items["a"])
	}

	if items["b"] != "b" {
		t.Fatalf("expected item b path to fall back to key, got %q", items["b"])
	}

	cm, err := clientset.CoreV1().ConfigMaps("ns").Get(context.Background(), "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap cm1 to be created: %v", err)
	}
	if cm.Name != "cm1" {
		t.Fatalf("expected created ConfigMap name cm1, got %q", cm.Name)
	}

	if controller.AddConfigMapIfMissing(deployment, config) {
		t.Fatalf("expected second call to be idempotent")
	}
}

func TestUpdateConfigMapIfMismatch_RemovesExtraKeys(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cm1",
			Namespace: "ns",
		},
		Data: map[string]string{
			"a":     "1",
			"extra": "2",
		},
	})

	controller := &Controller{
		clientset: clientset,
		logger:    zap.NewNop(),
	}

	err := controller.UpdateConfigMapIfMismatch("ns", "cm1", map[string]string{
		"a": "1",
	})
	if err != nil {
		t.Fatalf("UpdateConfigMapIfMismatch returned error: %v", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps("ns").Get(context.Background(), "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to fetch updated ConfigMap: %v", err)
	}

	if len(cm.Data) != 1 || cm.Data["a"] != "1" {
		t.Fatalf("expected ConfigMap data to be exactly map[a:1], got %#v", cm.Data)
	}
}

func TestEnsureConfigMapVolume_ReplacesNonConfigMapSource(t *testing.T) {
	volumes := []corev1.Volume{
		{
			Name: "cm1",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "old-secret",
				},
			},
		},
	}

	desiredItems := []corev1.KeyToPath{
		{Key: "app.conf", Path: "app.conf"},
	}

	if !ensureConfigMapVolume(&volumes, "cm1", desiredItems) {
		t.Fatalf("expected ensureConfigMapVolume to report change when existing source is not ConfigMap")
	}

	if len(volumes) != 1 {
		t.Fatalf("expected one volume, got %d", len(volumes))
	}

	if volumes[0].VolumeSource.Secret != nil {
		t.Fatalf("expected secret source to be removed")
	}

	if volumes[0].VolumeSource.ConfigMap == nil {
		t.Fatalf("expected ConfigMap source to be set")
	}

	if volumes[0].VolumeSource.ConfigMap.LocalObjectReference.Name != "cm1" {
		t.Fatalf("expected ConfigMap volume name cm1, got %q", volumes[0].VolumeSource.ConfigMap.LocalObjectReference.Name)
	}

	if !keyToPathSlicesEqual(volumes[0].VolumeSource.ConfigMap.Items, desiredItems) {
		t.Fatalf("expected ConfigMap items to match desired items")
	}

	if ensureConfigMapVolume(&volumes, "cm1", desiredItems) {
		t.Fatalf("expected second call to be idempotent")
	}
}

func TestEnsureSourceVolume_Secret(t *testing.T) {
	volumes := []corev1.Volume{}
	source := ConfigMapMount{
		Name:       "app-secret",
		SourceType: "Secret",
		Data:       []ConfigMapData{{Key: "token", SubPath: "token"}},
	}
	items := []corev1.KeyToPath{{Key: "token", Path: "token"}}

	if !ensureSourceVolume(&volumes, source, items) {
		t.Fatalf("expected ensureSourceVolume to add secret volume")
	}

	if len(volumes) != 1 || volumes[0].VolumeSource.Secret == nil {
		t.Fatalf("expected secret volume, got %#v", volumes)
	}
	if volumes[0].VolumeSource.Secret.SecretName != "app-secret" {
		t.Fatalf("unexpected secret name: %q", volumes[0].VolumeSource.Secret.SecretName)
	}

	if ensureSourceVolume(&volumes, source, items) {
		t.Fatalf("expected idempotent ensureSourceVolume call")
	}
}
