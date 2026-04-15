package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// main is the entry point of the operator.
func main() {
	kubeconfig := flag.String("kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "Path to the kubeconfig file")
	configFile := flag.String("config", "config.yaml", "Path to the configuration file")
	configCRDNamespace := flag.String("config-crd-namespace", "", "Namespace of WorkloadMountConfig (enables CRD config source when set with -config-crd-name)")
	configCRDName := flag.String("config-crd-name", "", "Name of WorkloadMountConfig (enables CRD config source when set with -config-crd-namespace)")
	flag.Parse()

	restConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		restConfig, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("Failed to initialize Kubernetes config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	loggerCfg := zap.NewProductionConfig()
	logger, err := loggerCfg.Build()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer func() {
		_ = logger.Sync()
	}()

	var controller *Controller
	crdMode := *configCRDNamespace != "" || *configCRDName != ""
	if crdMode {
		if *configCRDNamespace == "" || *configCRDName == "" {
			log.Fatalf("Both -config-crd-namespace and -config-crd-name must be set to use CRD config source")
		}

		dynamicClient, dynamicErr := dynamic.NewForConfig(restConfig)
		if dynamicErr != nil {
			log.Fatalf("Failed to initialize Kubernetes dynamic client: %v", dynamicErr)
		}

		controller, err = NewControllerFromCRD(clientset, dynamicClient, logger, *configCRDNamespace, *configCRDName)
		if err != nil {
			log.Fatalf("Failed to initialize controller from CRD: %v", err)
		}
	} else {
		controller, err = NewController(clientset, logger, *configFile)
		if err != nil {
			log.Fatalf("Failed to initialize controller from file config: %v", err)
		}
	}

	if err := controller.CheckAndUpdateConfigMapsFromConfig(); err != nil {
		logger.Error("Failed to update ConfigMaps from config", zap.Error(err))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("Starting deployment operator")
	if err := controller.Run(ctx); err != nil {
		logger.Fatal("Controller exited with error", zap.Error(err))
	}
}
