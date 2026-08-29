package collector

import (
	"context"
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DeploymentInfo is the workload metadata Consize persists.
type DeploymentInfo struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	RequestCPUMilli int64
	LimitCPUMilli   int64
	RequestMemBytes int64
	LimitMemBytes   int64
}

// MetadataClient resolves workload metadata from the cluster.
type MetadataClient interface {
	// ListDeployments returns every Deployment with its declared resources.
	ListDeployments(ctx context.Context) ([]DeploymentInfo, error)
	// PodOwners bulk-resolves pods to their owning Deployment. Keys are
	// "namespace/pod", values are Deployment names. Pods without a
	// Deployment owner (jobs, daemonsets, raw pods) are absent.
	PodOwners(ctx context.Context) (map[string]string, error)
}

// K8sMetadata implements MetadataClient with client-go.
type K8sMetadata struct {
	client     kubernetes.Interface
	namespaces []string // empty = cluster-wide; else per-namespace listing
}

// NewK8sMetadata builds a client from kubeconfig (or in-cluster when the
// config path is empty). When namespaces is non-empty, workload listing is
// scoped to those namespaces — matching per-namespace read Roles
// (ADR-025); empty means cluster-wide (the shipped default, which needs a
// cluster-scope read Role).
func NewK8sMetadata(kubeconfig string, namespaces []string) (*K8sMetadata, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		abs, aerr := filepath.Abs(kubeconfig)
		if aerr != nil {
			return nil, aerr
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", abs)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	return &K8sMetadata{client: clientset, namespaces: namespaces}, nil
}

// listNS returns the namespaces to list: the configured ones, or the whole
// cluster when none are configured.
func (k *K8sMetadata) listNS() []string {
	if len(k.namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return k.namespaces
}

// ListDeployments implements MetadataClient: every Deployment (or only
// those in the configured namespaces), with resources summed across its
// containers.
func (k *K8sMetadata) ListDeployments(ctx context.Context) ([]DeploymentInfo, error) {
	out := make([]DeploymentInfo, 0)
	for _, ns := range k.listNS() {
		all, err := k.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}
		for _, d := range all.Items {
			info := DeploymentInfo{Name: d.Name, Namespace: d.Namespace, Labels: d.Labels}
			for _, c := range d.Spec.Template.Spec.Containers {
				info.RequestCPUMilli += milliCPU(c.Resources.Requests[corev1.ResourceCPU])
				info.LimitCPUMilli += milliCPU(c.Resources.Limits[corev1.ResourceCPU])
				info.RequestMemBytes += c.Resources.Requests.Memory().Value()
				info.LimitMemBytes += c.Resources.Limits.Memory().Value()
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// PodOwners implements MetadataClient: one pass of three list calls.
// Pods typically own a ReplicaSet, which owns the Deployment — so we
// map RS → Deployment first, then pod → RS.
func (k *K8sMetadata) PodOwners(ctx context.Context) (map[string]string, error) {
	owners := map[string]string{}

	rsToDeploy := map[string]string{} // "ns/rs" → deployment
	for _, ns := range k.listNS() {
		rsList, err := k.client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list replicasets: %w", err)
		}
		for _, rs := range rsList.Items {
			for _, ref := range rs.OwnerReferences {
				if ref.Kind == "Deployment" && ref.APIVersion == "apps/v1" {
					rsToDeploy[rs.Namespace+"/"+rs.Name] = ref.Name
					break
				}
			}
		}
	}

	for _, ns := range k.listNS() {
		pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		for _, p := range pods.Items {
			for _, ref := range p.OwnerReferences {
				if ref.Kind != "ReplicaSet" {
					continue
				}
				if dep, ok := rsToDeploy[p.Namespace+"/"+ref.Name]; ok {
					owners[p.Namespace+"/"+p.Name] = dep
				}
				break
			}
		}
	}
	return owners, nil
}

// milliCPU converts a resource quantity (nanocores) to millicores.
// An absent key is the zero Quantity, i.e. 0.
func milliCPU(q resource.Quantity) int64 {
	return q.Value() / 1_000_000
}
