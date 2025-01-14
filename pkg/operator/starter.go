package operator

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/openshift/aws-efs-csi-driver-operator/assets"
	"github.com/openshift/aws-efs-csi-driver-operator/pkg/operator/staticresource"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	// Operand and operator run in the same namespace
	operatorName = "aws-efs-csi-driver-operator"

	namespaceReplaceKey = "${NAMESPACE}"
	// From credentials.yaml
	secretName = "aws-efs-cloud-credentials"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	operatorNamespace := controllerConfig.OperatorNamespace

	// Create core clientset and informer
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, operatorNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(operatorNamespace).Core().V1().Secrets()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)
	infraInformer := configInformers.Config().V1().Infrastructures()

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, string(opv1.AWSEFSCSIDriver))
	if err != nil {
		return err
	}

	// Dynamic client for CredentialsRequest
	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	cs := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithManagementStateController(
		operatorName,
		true,
	).WithLogLevelController().WithCSIDriverNodeService(
		"AWSEFSDriverNodeServiceController",
		replaceNamespaceFunc(operatorNamespace),
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace),
		nil,
	).WithCSIDriverControllerService(
		"AWSEFSDriverControllerServiceController",
		replaceNamespaceFunc(operatorNamespace),
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace),
		configInformers,
		[]factory.Informer{
			secretInformer.Informer(),
			nodeInformer.Informer(),
			infraInformer.Informer(),
		},
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(operatorNamespace, secretName, secretInformer),
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithReplicasHook(nodeInformer.Lister()),
	).WithCredentialsRequestController(
		"AWSEFSDriverCredentialsRequestController",
		operatorNamespace,
		replaceNamespaceFunc(operatorNamespace),
		"credentials.yaml",
		dynamicClient,
	).WithServiceMonitorController(
		"AWSEFSDriverServiceMonitorController",
		dynamicClient,
		replaceNamespaceFunc(operatorNamespace),
		"servicemonitor.yaml",
	)

	objsToSync := staticresource.SyncObjects{
		CSIDriver:                resourceread.ReadCSIDriverV1OrDie(mustReplaceNamespace(operatorNamespace, "csidriver.yaml")),
		PrivilegedRole:           resourceread.ReadClusterRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/privileged_role.yaml")),
		NodeServiceAccount:       resourceread.ReadServiceAccountV1OrDie(mustReplaceNamespace(operatorNamespace, "node_sa.yaml")),
		NodeRoleBinding:          resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/node_privileged_binding.yaml")),
		ControllerServiceAccount: resourceread.ReadServiceAccountV1OrDie(mustReplaceNamespace(operatorNamespace, "controller_sa.yaml")),
		ControllerRoleBinding:    resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/controller_privileged_binding.yaml")),
		ProvisionerRole:          resourceread.ReadClusterRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/provisioner_role.yaml")),
		ProvisionerRoleBinding:   resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/provisioner_binding.yaml")),
		PrometheusRole:           resourceread.ReadRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/prometheus_role.yaml")),
		PrometheusRoleBinding:    resourceread.ReadRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/prometheus_rolebinding.yaml")),
		MetricsService:           resourceread.ReadServiceV1OrDie(mustReplaceNamespace(operatorNamespace, "service.yaml")),
		RBACProxyRole:            resourceread.ReadClusterRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/kube_rbac_proxy_role.yaml")),
		RBACProxyRoleBinding:     resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/kube_rbac_proxy_binding.yaml")),
	}
	staticController := staticresource.NewCSIStaticResourceController(
		"CSIStaticResourceController",
		operatorNamespace,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		controllerConfig.EventRecorder,
		objsToSync,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go cs.Run(ctx, 1)
	go staticController.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func mustReplaceNamespace(namespace, file string) []byte {
	content, err := assets.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return bytes.Replace(content, []byte(namespaceReplaceKey), []byte(namespace), -1)
}

func replaceNamespaceFunc(namespace string) resourceapply.AssetFunc {
	return func(name string) ([]byte, error) {
		content, err := assets.ReadFile(name)
		if err != nil {
			panic(err)
		}
		return bytes.Replace(content, []byte(namespaceReplaceKey), []byte(namespace), -1), nil
	}
}
