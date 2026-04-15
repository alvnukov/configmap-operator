package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

const (
	resyncPeriod                 = 30 * time.Second
	requestTimeout               = 10 * time.Second
	workerCount                  = 2
	maxReconcileRetries          = 10
	deploymentQueueName          = "deployment-reconcile"
	podTemplateRolloutAnnotation = "configmap-operator.io/restarted-at"
	managedByLabelKey            = "app.kubernetes.io/managed-by"
	managedByLabelValue          = "configmap-operator"
)

// Controller is the operator controller.
type Controller struct {
	clientset kubernetes.Interface
	logger    *zap.Logger
	config    *DeploymentConfig
	queue     workqueue.RateLimitingInterface
}

// NewController creates a new controller from YAML config file.
func NewController(clientset kubernetes.Interface, logger *zap.Logger, configFile string) (*Controller, error) {
	if err := scheme.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to register client-go scheme: %w", err)
	}

	config, err := LoadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	return NewControllerWithConfig(clientset, logger, config), nil
}

// NewControllerWithConfig creates a new controller from in-memory config.
func NewControllerWithConfig(clientset kubernetes.Interface, logger *zap.Logger, config *DeploymentConfig) *Controller {
	return &Controller{
		clientset: clientset,
		logger:    logger,
		config:    config,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), deploymentQueueName),
	}
}

// Run starts informers and workers.
func (c *Controller) Run(ctx context.Context) error {
	sharedInformerFactory := informers.NewSharedInformerFactory(c.clientset, resyncPeriod)
	deploymentInformer := sharedInformerFactory.Apps().V1().Deployments().Informer()

	_, err := deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueDeployment,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment, okOld := oldObj.(*appsv1.Deployment)
			newDeployment, okNew := newObj.(*appsv1.Deployment)
			if !okOld || !okNew {
				c.logger.Warn("Failed to cast deployment update event")
				return
			}
			if oldDeployment.ResourceVersion == newDeployment.ResourceVersion {
				return
			}
			c.enqueueDeployment(newObj)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add deployment event handlers: %w", err)
	}

	sharedInformerFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), deploymentInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	c.enqueueConfiguredDeployments()

	for i := 0; i < workerCount; i++ {
		go c.runWorker(ctx)
	}

	<-ctx.Done()
	c.queue.ShutDown()
	c.logger.Info("Shutting down deployment operator...")
	return nil
}

func (c *Controller) enqueueConfiguredDeployments() {
	for _, deployment := range c.config.Deployments {
		key := deployment.Namespace + "/" + deployment.Name
		c.queue.Add(key)
	}
}

func (c *Controller) enqueueDeployment(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		c.logger.Error("Failed to build key for deployment event", zap.Error(err))
		return
	}
	c.queue.Add(key)
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(item)

	key, ok := item.(string)
	if !ok {
		c.queue.Forget(item)
		c.logger.Error("Queue item is not a string key")
		return true
	}

	if err := c.reconcileKey(ctx, key); err != nil {
		retries := c.queue.NumRequeues(key)
		if retries < maxReconcileRetries {
			c.queue.AddRateLimited(key)
			c.logger.Error("Failed to reconcile deployment",
				zap.String("key", key),
				zap.Int("retry", retries+1),
				zap.Error(err),
			)
		} else {
			c.queue.Forget(item)
			c.logger.Error("Dropping deployment after max retries",
				zap.String("key", key),
				zap.Int("retries", retries),
				zap.Error(err),
			)
		}
		return true
	}

	c.queue.Forget(item)
	return true
}

func (c *Controller) reconcileKey(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid queue key %q: %w", key, err)
	}

	deployment, found := c.config.FindDeployment(namespace, name)
	if !found {
		return nil
	}

	return c.reconcileDeployment(ctx, deployment)
}

func (c *Controller) reconcileDeployment(ctx context.Context, deploymentInfo *DeploymentInfo) error {
	if deploymentInfo == nil {
		return nil
	}

	configMapsChanged, err := c.reconcileConfigMaps(ctx, deploymentInfo)
	if err != nil {
		return err
	}

	desiredHash := desiredDeploymentHash(deploymentInfo)
	updated := false

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		getCtx, cancelGet := context.WithTimeout(ctx, requestTimeout)
		deployment, getErr := c.clientset.AppsV1().Deployments(deploymentInfo.Namespace).Get(getCtx, deploymentInfo.Name, metav1.GetOptions{})
		cancelGet()
		if getErr != nil {
			if kerrors.IsNotFound(getErr) {
				return nil
			}
			return getErr
		}

		mutableDeployment := deployment.DeepCopy()
		changesMade := c.applyDeploymentConfig(mutableDeployment, deploymentInfo)
		if ensurePodTemplateAnnotation(mutableDeployment, podTemplateConfigHashAnnotation, desiredHash) {
			changesMade = true
		}
		if configMapsChanged && ensurePodTemplateAnnotation(mutableDeployment, podTemplateRolloutAnnotation, strconv.FormatInt(time.Now().UnixNano(), 10)) {
			changesMade = true
		}

		if !changesMade {
			return nil
		}

		updateCtx, cancelUpdate := context.WithTimeout(ctx, requestTimeout)
		_, updateErr := c.clientset.AppsV1().Deployments(deploymentInfo.Namespace).Update(updateCtx, mutableDeployment, metav1.UpdateOptions{})
		cancelUpdate()
		if updateErr == nil {
			updated = true
		}
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile deployment %s/%s: %w", deploymentInfo.Namespace, deploymentInfo.Name, err)
	}

	if updated {
		c.logger.Info("Deployment reconciled",
			zap.String("namespace", deploymentInfo.Namespace),
			zap.String("name", deploymentInfo.Name),
		)
	}

	return nil
}

// AddConfigMapIfMissing is kept for compatibility and unit tests.
func (c *Controller) AddConfigMapIfMissing(deployment *appsv1.Deployment, config *DeploymentConfig) bool {
	if deployment == nil || config == nil {
		return false
	}

	deploymentInfo, found := config.FindDeployment(deployment.Namespace, deployment.Name)
	if !found {
		return false
	}

	changesMade := c.applyDeploymentConfig(deployment, deploymentInfo)
	reconciledConfigMaps := make(map[string]struct{})
	for _, container := range deploymentInfo.Containers {
		for _, configMap := range container.ConfigMaps {
			if _, exists := reconciledConfigMaps[configMap.Name]; exists {
				continue
			}
			reconciledConfigMaps[configMap.Name] = struct{}{}
			if err := c.UpdateConfigMapIfMismatch(deployment.Namespace, configMap.Name, getConfigMapData(configMap)); err != nil {
				c.logger.Error("Failed to reconcile ConfigMap in compatibility path",
					zap.String("namespace", deployment.Namespace),
					zap.String("configMap", configMap.Name),
					zap.Error(err),
				)
			}
		}
	}

	return changesMade
}

func (c *Controller) applyDeploymentConfig(deployment *appsv1.Deployment, deploymentInfo *DeploymentInfo) bool {
	if deployment == nil || deploymentInfo == nil {
		return false
	}

	changesMade := false

	for _, containerConfig := range deploymentInfo.Containers {
		containerIdx := findContainerIndex(deployment.Spec.Template.Spec.Containers, containerConfig.Name)
		if containerIdx < 0 {
			c.logger.Warn("Container not found in Deployment",
				zap.String("container", containerConfig.Name),
				zap.String("deployment", deployment.Name),
				zap.String("namespace", deployment.Namespace),
			)
			continue
		}

		container := &deployment.Spec.Template.Spec.Containers[containerIdx]

		for _, configMap := range containerConfig.ConfigMaps {
			desiredItems := buildConfigMapItems(configMap.Data, c.logger, deployment.Namespace, deployment.Name, configMap.Name)
			if ensureConfigMapVolume(&deployment.Spec.Template.Spec.Volumes, configMap.Name, desiredItems) {
				changesMade = true
			}

			for _, data := range configMap.Data {
				if !hasConfigMapVolumeMount(container.VolumeMounts, configMap.Name, data.MountPath, data.SubPath) {
					mount := corev1.VolumeMount{
						Name:      configMap.Name,
						MountPath: data.MountPath,
					}
					if data.SubPath != "" {
						mount.SubPath = data.SubPath
					}
					container.VolumeMounts = append(container.VolumeMounts, mount)
					changesMade = true
				}
			}
		}
	}

	return changesMade
}

func findContainerIndex(containers []corev1.Container, containerName string) int {
	for i, container := range containers {
		if container.Name == containerName {
			return i
		}
	}
	return -1
}

func buildConfigMapItems(data []ConfigMapData, logger *zap.Logger, namespace, deploymentName, configMapName string) []corev1.KeyToPath {
	items := make([]corev1.KeyToPath, 0, len(data))

	for _, item := range data {
		itemPath := item.SubPath
		if itemPath == "" {
			itemPath = item.Key
		}
		if item.Key == "" || itemPath == "" {
			logger.Warn("Skipping invalid ConfigMap item",
				zap.String("deployment", deploymentName),
				zap.String("namespace", namespace),
				zap.String("configMap", configMapName),
			)
			continue
		}

		items = append(items, corev1.KeyToPath{
			Key:  item.Key,
			Path: itemPath,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Key == items[j].Key {
			return items[i].Path < items[j].Path
		}
		return items[i].Key < items[j].Key
	})

	return items
}

func ensureConfigMapVolume(volumes *[]corev1.Volume, configMapName string, desiredItems []corev1.KeyToPath) bool {
	desiredSource := corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			Items:                cloneKeyToPathSlice(desiredItems),
		},
	}

	for i := range *volumes {
		volume := &(*volumes)[i]
		if volume.Name != configMapName {
			continue
		}

		if volume.VolumeSource.ConfigMap != nil &&
			volume.VolumeSource.ConfigMap.LocalObjectReference.Name == configMapName &&
			keyToPathSlicesEqual(volume.VolumeSource.ConfigMap.Items, desiredItems) {
			return false
		}

		volume.VolumeSource = desiredSource
		return true
	}

	*volumes = append(*volumes, corev1.Volume{
		Name: configMapName,
		VolumeSource: desiredSource,
	})
	return true
}

func keyToPathSlicesEqual(a, b []corev1.KeyToPath) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneKeyToPathSlice(items []corev1.KeyToPath) []corev1.KeyToPath {
	if len(items) == 0 {
		return nil
	}
	out := make([]corev1.KeyToPath, len(items))
	copy(out, items)
	return out
}

func hasConfigMapVolumeMount(volumeMounts []corev1.VolumeMount, configMapName, mountPath, subPath string) bool {
	for _, vm := range volumeMounts {
		if vm.Name == configMapName && vm.MountPath == mountPath && vm.SubPath == subPath {
			return true
		}
	}
	return false
}

func ensurePodTemplateAnnotation(deployment *appsv1.Deployment, key, value string) bool {
	annotations := deployment.Spec.Template.ObjectMeta.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
		deployment.Spec.Template.ObjectMeta.Annotations = annotations
	}
	if annotations[key] == value {
		return false
	}
	annotations[key] = value
	return true
}

// UpdateConfigMapIfMismatch checks ConfigMap state and updates it if needed.
func (c *Controller) UpdateConfigMapIfMismatch(namespace, name string, data map[string]string) error {
	_, err := c.ensureConfigMap(context.Background(), namespace, name, data)
	return err
}

// CheckAndUpdateConfigMapsFromConfig checks ConfigMaps against config and updates drift.
func (c *Controller) CheckAndUpdateConfigMapsFromConfig() error {
	for i := range c.config.Deployments {
		if _, err := c.reconcileConfigMaps(context.Background(), &c.config.Deployments[i]); err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) reconcileConfigMaps(ctx context.Context, deploymentInfo *DeploymentInfo) (bool, error) {
	if deploymentInfo == nil {
		return false, nil
	}

	desiredByName := make(map[string]map[string]string)
	for _, container := range deploymentInfo.Containers {
		for _, configMap := range container.ConfigMaps {
			desiredByName[configMap.Name] = getConfigMapData(configMap)
		}
	}

	names := make([]string, 0, len(desiredByName))
	for name := range desiredByName {
		names = append(names, name)
	}
	sort.Strings(names)

	anyChanges := false
	for _, name := range names {
		updated, err := c.ensureConfigMap(ctx, deploymentInfo.Namespace, name, desiredByName[name])
		if err != nil {
			return false, err
		}
		if updated {
			anyChanges = true
		}
	}

	return anyChanges, nil
}

func (c *Controller) ensureConfigMap(ctx context.Context, namespace, name string, desiredData map[string]string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("configMap name is empty")
	}

	changed := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		getCtx, cancelGet := context.WithTimeout(ctx, requestTimeout)
		configMap, getErr := c.clientset.CoreV1().ConfigMaps(namespace).Get(getCtx, name, metav1.GetOptions{})
		cancelGet()
		if getErr != nil {
			if !kerrors.IsNotFound(getErr) {
				return getErr
			}

			createCtx, cancelCreate := context.WithTimeout(ctx, requestTimeout)
			_, createErr := c.clientset.CoreV1().ConfigMaps(namespace).Create(createCtx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
					Labels: map[string]string{
						managedByLabelKey: managedByLabelValue,
					},
				},
				Data: cloneStringMap(desiredData),
			}, metav1.CreateOptions{})
			cancelCreate()
			if createErr != nil {
				return createErr
			}

			changed = true
			return nil
		}

		labels := cloneStringMap(configMap.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		if labels[managedByLabelKey] != managedByLabelValue {
			labels[managedByLabelKey] = managedByLabelValue
		}

		needsUpdate := !mapsEqual(configMap.Data, desiredData) || !mapsEqual(configMap.Labels, labels)
		if !needsUpdate {
			return nil
		}

		mutableConfigMap := configMap.DeepCopy()
		mutableConfigMap.Data = cloneStringMap(desiredData)
		mutableConfigMap.Labels = labels

		updateCtx, cancelUpdate := context.WithTimeout(ctx, requestTimeout)
		_, updateErr := c.clientset.CoreV1().ConfigMaps(namespace).Update(updateCtx, mutableConfigMap, metav1.UpdateOptions{})
		cancelUpdate()
		if updateErr != nil {
			return updateErr
		}

		changed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to reconcile ConfigMap %s/%s: %w", namespace, name, err)
	}

	if changed {
		c.logger.Info("ConfigMap reconciled",
			zap.String("namespace", namespace),
			zap.String("name", name),
		)
	}

	return changed, nil
}
