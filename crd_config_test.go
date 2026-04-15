package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeploymentConfigFromUnstructuredSpec_WorkloadMountsValid(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.alvnukov.dev/v1alpha1",
		"kind":       "WorkloadMountConfig",
		"spec": map[string]any{
			"deployments": []any{
				map[string]any{
					"namespace": "default",
					"name":      "demo",
					"containers": []any{
						map[string]any{
							"name": "app",
							"sources": []any{
								map[string]any{
									"type": "Secret",
									"name": "demo-secret",
									"items": []any{
										map[string]any{"key": "token", "value": "s3cr3t", "mountPath": "/etc/app/token", "subPath": "token"},
									},
								},
							},
						},
					},
				},
			},
		},
	}}

	cfg, err := deploymentConfigFromUnstructuredSpec(obj)
	if err != nil {
		t.Fatalf("deploymentConfigFromUnstructuredSpec() error = %v", err)
	}

	if len(cfg.Deployments) != 1 {
		t.Fatalf("expected one deployment, got %d", len(cfg.Deployments))
	}
	mount := cfg.Deployments[0].Containers[0].ConfigMaps[0]
	if mount.SourceType != "Secret" {
		t.Fatalf("expected SourceType Secret, got %q", mount.SourceType)
	}
}

func TestDeploymentConfigFromUnstructuredSpec_MissingSpec(t *testing.T) {
	_, err := deploymentConfigFromUnstructuredSpec(&unstructured.Unstructured{Object: map[string]any{}})
	if err == nil {
		t.Fatalf("expected error for missing spec")
	}
	if !strings.Contains(err.Error(), "spec is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
