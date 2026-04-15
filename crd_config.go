package main

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	crdGroup    = "operator.alvnukov.dev"
	crdVersion  = "v1alpha1"
	crdResource = "workloadmountconfigs"
)

var configGVR = schema.GroupVersionResource{Group: crdGroup, Version: crdVersion, Resource: crdResource}

// LoadConfigFromCRD loads and validates operator config from WorkloadMountConfig.spec.
func LoadConfigFromCRD(ctx context.Context, dynamicClient dynamic.Interface, namespace, name string) (*DeploymentConfig, error) {
	if dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("crd namespace is required")
	}
	if name == "" {
		return nil, fmt.Errorf("crd name is required")
	}

	obj, err := dynamicClient.Resource(configGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get WorkloadMountConfig %s/%s: %w", namespace, name, err)
	}

	return deploymentConfigFromUnstructuredSpec(obj)
}

type workloadMountSpec struct {
	Deployments []workloadMountDeployment `json:"deployments"`
}

type workloadMountDeployment struct {
	Namespace  string                   `json:"namespace"`
	Name       string                   `json:"name"`
	Containers []workloadMountContainer `json:"containers"`
}

type workloadMountContainer struct {
	Name    string                `json:"name"`
	Sources []workloadMountSource `json:"sources"`
}

type workloadMountSource struct {
	Type  string              `json:"type"`
	Name  string              `json:"name"`
	Items []workloadMountItem `json:"items"`
}

type workloadMountItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath"`
	Path      string `json:"path"`
}

func deploymentConfigFromUnstructuredSpec(obj *unstructured.Unstructured) (*DeploymentConfig, error) {
	if obj == nil {
		return nil, fmt.Errorf("config object is nil")
	}

	specMap, ok, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("failed to read spec from WorkloadMountConfig: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("WorkloadMountConfig.spec is required")
	}

	spec := &workloadMountSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(specMap, spec); err != nil {
		return nil, fmt.Errorf("failed to decode WorkloadMountConfig.spec: %w", err)
	}

	cfg := &DeploymentConfig{Deployments: make([]DeploymentInfo, 0, len(spec.Deployments))}
	for _, dep := range spec.Deployments {
		depInfo := DeploymentInfo{
			Namespace:  strings.TrimSpace(dep.Namespace),
			Name:       strings.TrimSpace(dep.Name),
			Containers: make([]ContainerConfig, 0, len(dep.Containers)),
		}

		for _, c := range dep.Containers {
			container := ContainerConfig{
				Name:       strings.TrimSpace(c.Name),
				ConfigMaps: make([]ConfigMapMount, 0, len(c.Sources)),
			}

			for _, s := range c.Sources {
				sourceType := strings.TrimSpace(s.Type)
				if sourceType == "" {
					sourceType = "ConfigMap"
				}

				mount := ConfigMapMount{
					Name:       strings.TrimSpace(s.Name),
					SourceType: sourceType,
					Data:       make([]ConfigMapData, 0, len(s.Items)),
				}

				for _, item := range s.Items {
					subPath := strings.TrimSpace(item.SubPath)
					if subPath == "" {
						subPath = strings.TrimSpace(item.Path)
					}
					mount.Data = append(mount.Data, ConfigMapData{
						Key:       strings.TrimSpace(item.Key),
						Value:     item.Value,
						SubPath:   subPath,
						MountPath: strings.TrimSpace(item.MountPath),
					})
				}

				container.ConfigMaps = append(container.ConfigMaps, mount)
			}

			depInfo.Containers = append(depInfo.Containers, container)
		}

		cfg.Deployments = append(cfg.Deployments, depInfo)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
