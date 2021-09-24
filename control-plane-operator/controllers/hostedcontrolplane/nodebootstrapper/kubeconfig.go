package nodebootstrapper

import (
	"context"
	"fmt"

	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
)

const (
	KubeconfigKey                  = "kubeconfig"
	machineConfigOperatorNamespace = "openshift-machine-config-operator"
)

// ReconcileBootstrapKubeconfig maintains a secret named bootstrap-kubeconfig. This secret will be used in kubelet client CSR creation.
// The fucntion uses a kubeconfig in the service-network-admin-kubeconfig secret to connect to the hosted cluster.
// On the hosted cluster, a node-bootstrapper service account in the openshift-machine-config-operator namespace is created.
// The service account token will be used to generate a kubeconfig named bootstrap-kubeconfig on the management cluster, which kubelet will use for client csr.
func ReconcileBootstrapKubeconfig(mangementClient client.Client, ctx context.Context, hcp *hyperv1.HostedControlPlane) error {
	if hcp.Status.KubeConfig == nil {
		return fmt.Errorf("hosted controlplane kubeconfig secret is not generated")
	}
	// Resolve the kubeconfig secret for the hosted control plane
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hcp.Namespace,
			Name:      "service-network-admin-kubeconfig",
		},
	}
	err := mangementClient.Get(ctx, client.ObjectKeyFromObject(kubeconfigSecret), kubeconfigSecret)
	if err != nil {
		return fmt.Errorf("failed to get hosted controlplane kubeconfig secret %q: %w", kubeconfigSecret.Name, err)
	}

	// construct client for hosted controlplane
	var config *clientcmdapi.Config

	if kubeconfig, ok := kubeconfigSecret.Data[KubeconfigKey]; ok {
		config, err = clientcmd.Load(kubeconfig)
		if err != nil {
			return err
		}
	}
	if config == nil {
		return fmt.Errorf("failed to load hosted controlplane kubeconfig from secret %q: %w", kubeconfigSecret.Name, err)
	}
	clientConfig, err := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to create hosted controlplane client from secret %q: %w", kubeconfigSecret.Name, err)
	}
	hostedClusterClient, err := client.New(clientConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create hosted controlplane client: %w", err)
	}

	ns := manifests.BootstrapMachineConfigNamespace(machineConfigOperatorNamespace)
	_, err = controllerutil.CreateOrUpdate(ctx, hostedClusterClient, ns, func() error { return nil })
	if err != nil {
		return fmt.Errorf("failed to reconcile machine-config-operator namespace: %w", err)
	}

	// Reconcile bootstrapper service account
	sa := manifests.BootstrapServiceAccount(machineConfigOperatorNamespace)
	_, err = controllerutil.CreateOrUpdate(ctx, hostedClusterClient, sa, func() error { return nil })
	if err != nil {
		return fmt.Errorf("failed to reconcile node bootstrapper service account: %w", err)
	}

	// Reconcile bootstrapper role binding
	rolebinding := manifests.BootstrapClusterRoleBinding()
	_, err = controllerutil.CreateOrUpdate(ctx, hostedClusterClient, rolebinding, func() error {
		return reconcileBootstrapClusterRoleBinding(rolebinding, manifests.BootstrapClusterRole(), sa)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile bootstrapper role binding: %w", err)
	}
	// Reconcile bootstrapper token secret
	tokenSecret := manifests.BootstrapServicAccountTokenSecret(machineConfigOperatorNamespace)
	_, err = controllerutil.CreateOrPatch(ctx, hostedClusterClient, tokenSecret, func() error {
		if tokenSecret.Annotations == nil {
			tokenSecret.Annotations = map[string]string{}
		}
		tokenSecret.Annotations["kubernetes.io/service-account.name"] = sa.Name
		tokenSecret.Type = corev1.SecretTypeServiceAccountToken
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile bootstrapper service account token secret: %w", err)
	}

	// Resolve the kubeconfig secret for bootstrapper sa token
	err = hostedClusterClient.Get(ctx, client.ObjectKeyFromObject(tokenSecret), tokenSecret)
	if err != nil {
		return fmt.Errorf("failed to get bootstrapper service account token secret %q: %w", tokenSecret.Name, err)
	}
	if tokenSecret.Data[corev1.ServiceAccountRootCAKey] == nil || tokenSecret.Data[corev1.ServiceAccountTokenKey] == nil {
		return fmt.Errorf("failed to get bootstrapper service account token from secret %q: %w", tokenSecret.Name, err)
	}
	if hcp.Status.ControlPlaneEndpoint.Host == "" {
		return fmt.Errorf("failed to get apiserver url from hcp.Status.ControlPlaneEndpoint: %v", hcp.Status.ControlPlaneEndpoint.Host)
	}
	apiServerURL := fmt.Sprintf("https://%s:%d", hcp.Status.ControlPlaneEndpoint.Host, hcp.Status.ControlPlaneEndpoint.Port)
	// generate kubeconfig from the token secret
	kcData, _, err := kubeconfigFromSecret(tokenSecret, apiServerURL)
	if err != nil {
		return fmt.Errorf("failed to generate kubeconfig from secret %q: %w", tokenSecret.Name, err)
	}

	bootstrapKubeconfigSecret := manifests.BootstrapKubeconfigSecret(hcp.Namespace)
	_, err = controllerutil.CreateOrUpdate(ctx, mangementClient, bootstrapKubeconfigSecret, func() error {
		return reconcileBootstrapKubeconfigSecret(bootstrapKubeconfigSecret, kcData, KubeconfigKey)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile bootstrapper kubeconfig %q: %w", bootstrapKubeconfigSecret.Name, err)
	}
	return nil
}

func reconcileBootstrapClusterRoleBinding(binding *rbacv1.ClusterRoleBinding, role *rbacv1.ClusterRole, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
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

func reconcileBootstrapKubeconfigSecret(secret *corev1.Secret, kcData []byte, key string) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if key == "" {
		key = "kubeconfig"
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	//secret.Labels[kas.KubeconfigScopeLabel] = string(scope)
	secret.Labels["hypershift.openshift.io/kubeconfig"] = "bootstrap"
	secret.Data[key] = kcData
	return nil
}

func kubeconfigFromSecret(secret *corev1.Secret, apiserverURL string) ([]byte, []byte, error) {
	caData := secret.Data[corev1.ServiceAccountRootCAKey]
	token := secret.Data[corev1.ServiceAccountTokenKey]

	kubeconfig := clientcmdv1.Config{
		Clusters: []clientcmdv1.NamedCluster{{
			Name: "local",
			Cluster: clientcmdv1.Cluster{
				Server:                   apiserverURL,
				CertificateAuthorityData: caData,
			}},
		},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name: "kubelet",
			AuthInfo: clientcmdv1.AuthInfo{
				Token: string(token),
			},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name: "kubelet",
			Context: clientcmdv1.Context{
				Cluster:  "local",
				AuthInfo: "kubelet",
			},
		}},
		CurrentContext: "kubelet",
	}
	kcData, err := yaml.Marshal(kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	return kcData, caData, nil
}
