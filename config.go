package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const podTemplateConfigHashAnnotation = "configmap-operator.io/config-hash"

// ConfigMapData represents data for a ConfigMap item.
type ConfigMapData struct {
	Key       string `yaml:"key"`
	Value     string `yaml:"value"`
	SubPath   string `yaml:"subPath"`
	MountPath string `yaml:"mountPath"`
}

// ConfigMapMount represents a ConfigMap and its data to be mounted in a container.
type ConfigMapMount struct {
	Name       string          `yaml:"name"`
	SourceType string          `yaml:"sourceType"`
	Data       []ConfigMapData `yaml:"data"`
}

// ContainerConfig represents a container and its ConfigMap mounts.
type ContainerConfig struct {
	Name       string           `yaml:"name"`
	ConfigMaps []ConfigMapMount `yaml:"configMaps"`
}

// DeploymentInfo represents deployment information.
type DeploymentInfo struct {
	Namespace  string            `yaml:"namespace"`
	Name       string            `yaml:"name"`
	Containers []ContainerConfig `yaml:"containers"`
}

// DeploymentConfig represents the configuration for deployments.
type DeploymentConfig struct {
	Deployments []DeploymentInfo `yaml:"deployments"`
}

// LoadConfig loads and validates operator configuration from file.
func LoadConfig(filename string) (*DeploymentConfig, error) {
	// #nosec G304 -- configuration file path is an explicit operator CLI input.
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	return ParseConfig(data)
}

// ParseConfig parses YAML in strict mode and validates semantic constraints.
func ParseConfig(data []byte) (*DeploymentConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	cfg := &DeploymentConfig{}
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	var extraDoc any
	if err := decoder.Decode(&extraDoc); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to decode trailing YAML document: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate validates operator config and returns an aggregated error when invalid.
func (c *DeploymentConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if len(c.Deployments) == 0 {
		return fmt.Errorf("invalid config: at least one deployment must be configured")
	}

	errs := make([]string, 0)
	deploymentSeen := make(map[string]struct{}, len(c.Deployments))

	for di := range c.Deployments {
		deployment := &c.Deployments[di]
		deployment.Namespace = strings.TrimSpace(deployment.Namespace)
		deployment.Name = strings.TrimSpace(deployment.Name)

		if deployment.Namespace == "" {
			errs = append(errs, fmt.Sprintf("deployments[%d].namespace is required", di))
		}
		if deployment.Name == "" {
			errs = append(errs, fmt.Sprintf("deployments[%d].name is required", di))
		}

		deploymentKey := deployment.Namespace + "/" + deployment.Name
		if _, exists := deploymentSeen[deploymentKey]; exists {
			errs = append(errs, fmt.Sprintf("duplicate deployment %q", deploymentKey))
		}
		deploymentSeen[deploymentKey] = struct{}{}
		deploymentSourceDataSeen := make(map[string]map[string]string)

		containerSeen := make(map[string]struct{}, len(deployment.Containers))
		for ci := range deployment.Containers {
			container := &deployment.Containers[ci]
			container.Name = strings.TrimSpace(container.Name)
			if container.Name == "" {
				errs = append(errs, fmt.Sprintf("deployments[%d].containers[%d].name is required", di, ci))
			}
			if _, exists := containerSeen[container.Name]; exists {
				errs = append(errs, fmt.Sprintf("duplicate container %q in deployment %q", container.Name, deploymentKey))
			}
			containerSeen[container.Name] = struct{}{}
			containerMountTargetSeen := make(map[string]string)

			configMapSeen := make(map[string]struct{}, len(container.ConfigMaps))
			for mi := range container.ConfigMaps {
				configMap := &container.ConfigMaps[mi]
				configMap.Name = strings.TrimSpace(configMap.Name)
				configMap.SourceType = strings.TrimSpace(configMap.SourceType)
				if configMap.SourceType == "" {
					configMap.SourceType = "ConfigMap"
				}
				if configMap.SourceType != "ConfigMap" && configMap.SourceType != "Secret" {
					errs = append(errs, fmt.Sprintf("unsupported sourceType %q in deployment %q container %q source %q (allowed: ConfigMap, Secret)", configMap.SourceType, deploymentKey, container.Name, configMap.Name))
				}
				if configMap.Name == "" {
					errs = append(errs, fmt.Sprintf("deployments[%d].containers[%d].configMaps[%d].name is required", di, ci, mi))
				}
				if _, exists := configMapSeen[configMap.Name]; exists {
					errs = append(errs, fmt.Sprintf("duplicate configMap %q in deployment %q container %q", configMap.Name, deploymentKey, container.Name))
				}
				configMapSeen[configMap.Name] = struct{}{}
				configMapData := make(map[string]string, len(configMap.Data))

				dataKeySeen := make(map[string]struct{}, len(configMap.Data))
				mountTargetSeen := make(map[string]struct{}, len(configMap.Data))
				for vi := range configMap.Data {
					item := &configMap.Data[vi]
					item.Key = strings.TrimSpace(item.Key)
					item.SubPath = strings.TrimSpace(item.SubPath)
					item.MountPath = strings.TrimSpace(item.MountPath)

					if item.Key == "" {
						errs = append(errs, fmt.Sprintf("deployments[%d].containers[%d].configMaps[%d].data[%d].key is required", di, ci, mi, vi))
					}
					if item.MountPath == "" {
						errs = append(errs, fmt.Sprintf("deployments[%d].containers[%d].configMaps[%d].data[%d].mountPath is required", di, ci, mi, vi))
					}

					if _, exists := dataKeySeen[item.Key]; exists {
						errs = append(errs, fmt.Sprintf("duplicate key %q in deployment %q container %q configMap %q", item.Key, deploymentKey, container.Name, configMap.Name))
					}
					dataKeySeen[item.Key] = struct{}{}
					if item.Key != "" {
						configMapData[item.Key] = item.Value
					}

					mountKey := item.MountPath + "\x00" + item.SubPath
					if _, exists := mountTargetSeen[mountKey]; exists {
						errs = append(errs, fmt.Sprintf("duplicate mount target mountPath=%q subPath=%q in deployment %q container %q configMap %q", item.MountPath, item.SubPath, deploymentKey, container.Name, configMap.Name))
					}
					mountTargetSeen[mountKey] = struct{}{}
					if previousConfigMap, exists := containerMountTargetSeen[mountKey]; exists && previousConfigMap != configMap.Name {
						errs = append(errs, fmt.Sprintf("mount target mountPath=%q subPath=%q is used by multiple configMaps (%q, %q) in deployment %q container %q", item.MountPath, item.SubPath, previousConfigMap, configMap.Name, deploymentKey, container.Name))
					} else {
						containerMountTargetSeen[mountKey] = configMap.Name
					}
				}

				if configMap.Name == "" {
					continue
				}

				sourceKey := configMap.SourceType + ":" + configMap.Name
				if previousData, exists := deploymentSourceDataSeen[sourceKey]; exists {
					if !mapsEqual(previousData, configMapData) {
						errs = append(errs, fmt.Sprintf("source %q has conflicting data definitions between containers in deployment %q", sourceKey, deploymentKey))
					}
				} else {
					deploymentSourceDataSeen[sourceKey] = cloneStringMap(configMapData)
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}

	return nil
}

// FindDeployment returns deployment config by namespaced name.
func (c *DeploymentConfig) FindDeployment(namespace, name string) (*DeploymentInfo, bool) {
	for i := range c.Deployments {
		deployment := &c.Deployments[i]
		if deployment.Namespace == namespace && deployment.Name == name {
			return deployment, true
		}
	}
	return nil, false
}

func getConfigMapData(configMap ConfigMapMount) map[string]string {
	data := make(map[string]string, len(configMap.Data))
	for _, cmData := range configMap.Data {
		if cmData.Key == "" {
			continue
		}
		data[cmData.Key] = cmData.Value
	}
	return data
}

func desiredDeploymentHash(deployment *DeploymentInfo) string {
	if deployment == nil {
		return ""
	}

	hasher := sha256.New()
	records := make([]string, 0)
	records = append(records, "deployment\x00"+deployment.Namespace+"\x00"+deployment.Name)

	for _, container := range deployment.Containers {
		if len(container.ConfigMaps) == 0 {
			records = append(records, "container\x00"+container.Name+"\x00<no-configmaps>")
			continue
		}

		for _, configMap := range container.ConfigMaps {
			if len(configMap.Data) == 0 {
				records = append(records, strings.Join([]string{
					"entry",
					container.Name,
					configMap.Name,
					"<no-data>",
				}, "\x00"))
				continue
			}

			for _, item := range configMap.Data {
				records = append(records, strings.Join([]string{
					"entry",
					container.Name,
					configMap.Name,
					item.Key,
					item.Value,
					item.MountPath,
					item.SubPath,
				}, "\x00"))
			}
		}
	}

	sort.Strings(records)
	for _, record := range records {
		hasher.Write([]byte(record))
		hasher.Write([]byte("\n"))
	}

	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
