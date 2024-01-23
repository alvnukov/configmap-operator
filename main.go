package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const resyncPeriod = 30 * time.Second

// ConfigMapData represents data for a ConfigMap item
type ConfigMapData struct {
	Key       string `yaml:"key"`
	Value     string `yaml:"value"`
	SubPath   string `yaml:"subPath"`
	MountPath string `yaml:"mountPath"`
}

// ConfigMapMount represents a ConfigMap and its data to be mounted in a container
type ConfigMapMount struct {
	Name string          `yaml:"name"`
	Data []ConfigMapData `yaml:"data"`
}

// ContainerConfig represents a container and its ConfigMap mounts
type ContainerConfig struct {
	Name       string           `yaml:"name"`
	ConfigMaps []ConfigMapMount `yaml:"configMaps"`
}

// DeploymentInfo represents deployment information
type DeploymentInfo struct {
	Namespace  string            `yaml:"namespace"`
	Name       string            `yaml:"name"`
	Containers []ContainerConfig `yaml:"containers"`
}

// DeploymentConfig represents the configuration for deployments
type DeploymentConfig struct {
	Deployments []DeploymentInfo `yaml:"deployments"`
}

// LoadConfig loads the configuration from a YAML file
func LoadConfig(filename string) (*DeploymentConfig, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	config := &DeploymentConfig{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, err
	}

	return config, nil
}

// Controller is the main struct for the operator controller
type Controller struct {
	clientset *kubernetes.Clientset
	logger    *zap.Logger
	config    *DeploymentConfig
}

// NewController creates a new instance of Controller
func NewController(clientset *kubernetes.Clientset, logger *zap.Logger, configFile *string) *Controller {
	scheme.AddToScheme(scheme.Scheme)

	config, err := LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	return &Controller{
		clientset: clientset,
		logger:    logger,
		config:    config,
	}
}

// hasConfigMapVolume checks if a ConfigMap volume with the given name exists in the list of volumes
func hasConfigMapVolume(volume []corev1.Volume, name string) bool {
	for _, vm := range volume {
		if vm.Name == name {
			return true
		}
	}
	return false
}

func hasConfigMapVolumeMount(volumeMounts []corev1.VolumeMount, configMapName string) bool {
	for _, vm := range volumeMounts {
		if vm.Name == configMapName {
			return true
		}
	}
	return false
}

func (c *Controller) AddConfigMapIfMissing(deployment *appsv1.Deployment, config *DeploymentConfig) bool {
	changesMade := false

	for _, deploymentInfo := range config.Deployments {
		if deployment.Namespace == deploymentInfo.Namespace && deployment.Name == deploymentInfo.Name {
			for _, containerConfig := range deploymentInfo.Containers {
				for _, configMap := range containerConfig.ConfigMaps {
					containerFound := false
					for i, container := range deployment.Spec.Template.Spec.Containers {
						if container.Name == containerConfig.Name {
							containerFound = true
							for _, data := range configMap.Data {
								if !hasConfigMapVolumeMount(container.VolumeMounts, configMap.Name) {
									changesMade = true // ConfigMap mount added, mark as changes made
									volumeMount := corev1.VolumeMount{
										Name:      configMap.Name,
										MountPath: data.MountPath,
									}

									if data.SubPath != "" {
										volumeMount.SubPath = data.SubPath
									}

									deployment.Spec.Template.Spec.Containers[i].VolumeMounts = append(deployment.Spec.Template.Spec.Containers[i].VolumeMounts, volumeMount)
								}
							}
						}
					}

					if !containerFound {
						c.logger.Warn("Container not found in Deployment",
							zap.String("container", containerConfig.Name),
							zap.String("deployment", deployment.Name),
							zap.String("namespace", deployment.Namespace),
						)
					}

					if !hasConfigMapVolume(deployment.Spec.Template.Spec.Volumes, configMap.Name) {
						changesMade = true // ConfigMap volume added, mark as changes made
						volumeSource := corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: configMap.Name,
								},
							},
						}

						if len(configMap.Data) > 0 {
							volumeSource.ConfigMap.Items = make([]corev1.KeyToPath, 0, len(configMap.Data))
							for _, data := range configMap.Data {
								volumeSource.ConfigMap.Items = append(volumeSource.ConfigMap.Items, corev1.KeyToPath{
									Key:  data.Key,
									Path: data.SubPath,
								})
							}
						}

						volume := corev1.Volume{
							Name:         configMap.Name,
							VolumeSource: volumeSource,
						}

						deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, volume)

						// Create the ConfigMap if it doesn't exist
						_, err := c.clientset.CoreV1().ConfigMaps(deployment.Namespace).Create(context.TODO(), &corev1.ConfigMap{
							ObjectMeta: metav1.ObjectMeta{
								Name: configMap.Name,
							},
							Data: nil,
						}, metav1.CreateOptions{})

						if err != nil && !kerrors.IsAlreadyExists(err) {
							c.logger.Error("Failed to create ConfigMap in the cluster", zap.Error(err))
						}
					}
				}
			}
		}
	}

	return changesMade
}

// UpdateConfigMapIfMismatch checks if an existing ConfigMap matches the specified data
// and updates it if necessary
func (c *Controller) UpdateConfigMapIfMismatch(namespace, name string, data map[string]string) error {
	configMap, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			// ConfigMap not found, create it
			newConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Data: data,
			}

			_, err = c.clientset.CoreV1().ConfigMaps(namespace).Create(context.Background(), newConfigMap, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create ConfigMap: %w", err)
			}

			c.logger.Info("Created ConfigMap",
				zap.String("namespace", namespace),
				zap.String("name", name),
			)
			return nil
		}

		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Check if the data in the existing ConfigMap matches the specified data
	needsUpdate := false
	for key, value := range data {
		if configMap.Data[key] != value {
			needsUpdate = true
			break
		}
	}

	// If the ConfigMap needs an update, update its data
	if needsUpdate {
		configMap.Data = data
		_, err := c.clientset.CoreV1().ConfigMaps(namespace).Update(context.Background(), configMap, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update ConfigMap: %w", err)
		}

		c.logger.Info("ConfigMap updated",
			zap.String("namespace", namespace),
			zap.String("name", name),
		)
	}

	return nil
}

// CheckAndUpdateConfigMapsFromConfig checks the existing ConfigMaps against the configuration file
// and updates them if necessary
func (c *Controller) CheckAndUpdateConfigMapsFromConfig() error {
	for _, deploymentInfo := range c.config.Deployments {
		for _, containerConfig := range deploymentInfo.Containers {
			for _, configMap := range containerConfig.ConfigMaps {
				if configMap.Name != "" && len(configMap.Data) > 0 {
					err := c.UpdateConfigMapIfMismatch(deploymentInfo.Namespace, configMap.Name, getConfigMapData(configMap))
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// getConfigMapData returns the data map for a ConfigMap
func getConfigMapData(configMap ConfigMapMount) map[string]string {
	data := make(map[string]string)
	for _, cmData := range configMap.Data {
		data[cmData.Key] = cmData.Value
	}
	return data
}

// main is the entry point of the operator
func main() {
	kubeconfig := flag.String("kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "Path to the kubeconfig file")
	configFile := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	cfg := zap.NewProductionConfig()
	logger, err := cfg.Build()

	defer logger.Sync()

	controller := NewController(clientset, logger, configFile)

	// Check existing ConfigMaps against the configuration file and update them if necessary
	if err := controller.CheckAndUpdateConfigMapsFromConfig(); err != nil {
		logger.Error("Failed to update ConfigMaps from config", zap.Error(err))
	}

	// Create a context to manage the operator's shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start signal handling for graceful shutdown
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalCh
		logger.Info("Received interrupt signal, shutting down...")
		cancel()
	}()

	fmt.Println("Starting deployment operator...")
	controller.Run(ctx)
}

// Run starts the operator controller
func (c *Controller) Run(ctx context.Context) {
	sharedInformerFactory := informers.NewSharedInformerFactory(c.clientset, resyncPeriod)
	deploymentInformer := sharedInformerFactory.Apps().V1().Deployments().Informer()

	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			deployment := obj.(*appsv1.Deployment)
			c.logger.Debug("New deployment added",
				zap.String("namespace", deployment.Namespace),
				zap.String("name", deployment.Name),
			)

			changesMade := c.AddConfigMapIfMissing(deployment, c.config)

			// Save the changes to the cluster if there were any changes made
			if changesMade {
				updatedDeployment, err := c.clientset.AppsV1().Deployments(deployment.Namespace).Update(ctx, deployment, metav1.UpdateOptions{})
				if err != nil {
					c.logger.Error("Failed to update deployment in the cluster", zap.Error(err),
						zap.String("resourceVersion", updatedDeployment.ResourceVersion),
					)
					return
				}

				c.logger.Info("Deployment updated in the cluster",
					zap.String("namespace", updatedDeployment.Namespace),
					zap.String("name", updatedDeployment.Name),
					zap.String("resourceVersion", updatedDeployment.ResourceVersion),
				)
			} else {
				c.logger.Debug("No changes made to the deployment",
					zap.String("namespace", deployment.Namespace),
					zap.String("name", deployment.Name),
				)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment := oldObj.(*appsv1.Deployment)
			newDeployment := newObj.(*appsv1.Deployment)

			if oldDeployment.ResourceVersion == newDeployment.ResourceVersion {
				// The deployment hasn't changed, no need to process it
				return
			}

			changesMade := c.AddConfigMapIfMissing(newDeployment, c.config)
			// Save the changes to the cluster if there were any changes made
			if changesMade {
				updatedDeployment, err := c.clientset.AppsV1().Deployments(newDeployment.Namespace).Update(ctx, newDeployment, metav1.UpdateOptions{})
				if err != nil {
					c.logger.Error("Failed to update deployment in the cluster", zap.Error(err),
						zap.String("resourceVersion", updatedDeployment.ResourceVersion),
					)
					return
				}

				c.logger.Info("Deployment updated in the cluster",
					zap.String("namespace", updatedDeployment.Namespace),
					zap.String("name", updatedDeployment.Name),
					zap.String("resourceVersion", updatedDeployment.ResourceVersion),
				)
			} else {
				c.logger.Debug("No changes made to the deployment",
					zap.String("namespace", newDeployment.Namespace),
					zap.String("name", newDeployment.Name),
				)
			}
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)
	sharedInformerFactory.Start(stopCh)

	if !cache.WaitForCacheSync(ctx.Done(), deploymentInformer.HasSynced) {
		fmt.Println("Timed out waiting for caches to sync")
		return
	}

	<-ctx.Done()
	c.logger.Info("Shutting down deployment operator...")
}
