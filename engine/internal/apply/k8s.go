package apply

import (
	"context"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"consize/internal/store"
)

// K8sPatcher implements Patcher against the real cluster.
type K8sPatcher struct {
	client kubernetes.Interface
}

// NewK8sPatcher builds a patcher from kubeconfig (or in-cluster when
// empty). Must be a WRITE ServiceAccount (docs/security.md): the
// collector's read-only identity can never apply.
func NewK8sPatcher(kubeconfig string) (*K8sPatcher, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
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
	return &K8sPatcher{client: clientset}, nil
}

// Health reports cluster reachability (the API's readyz gates applies
// on store + cluster).
func (k *K8sPatcher) Health(ctx context.Context) error {
	_, err := k.client.Discovery().ServerVersion()
	return err
}

// ReadResources implements Patcher: the deployment's aggregate request
// and limit totals for one resource kind, summed across containers —
// the same aggregate contract a store.Diff carries.
func (k *K8sPatcher) ReadResources(ctx context.Context, namespace, name, kind string) (req, lim int64, err error) {
	dep, err := k.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("get deployment: %w", err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		r, l := fieldValues(kind, c.Resources.Requests, c.Resources.Limits)
		req += r
		lim += l
	}
	return req, lim, nil
}

// PatchDeployment implements Patcher: GET the deployment, distribute the
// resource delta across containers proportionally to each container's
// current request share, UPDATE with the resourceVersion we read
// (conflicts retried on a fresh read). The rollout proceeds through the
// deployment's normal ReplicaSet machinery — Consize never touches pods
// directly.
func (k *K8sPatcher) PatchDeployment(ctx context.Context, namespace, name string, d store.Diff) error {
	for attempt := 0; attempt < 3; attempt++ {
		dep, err := k.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment: %w", err)
		}
		if !mutateResources(dep.Spec.Template.Spec.Containers, d) {
			return fmt.Errorf("no container to patch in %s/%s", namespace, name)
		}
		_, err = k.client.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) {
			// Another actor (or a rollout) advanced the deployment;
			// re-read and re-apply the same intent.
			continue
		}
		return fmt.Errorf("update deployment: %w", err)
	}
	return fmt.Errorf("patch %s/%s: resourceVersion conflicted 3 times", namespace, name)
}

// containerShare is one container's slice of a resource delta.
type containerShare struct {
	idx    int
	hasReq bool
	hasLim bool
	curReq int64
	share  float64
}

// mutateResources applies the request/limit delta of one resource kind
// across the pod template's containers, weighted by each container's
// current request share; the last container absorbs rounding so the
// totals land exactly on the proposed values.
//
// QoS rule: a container keeps whatever resource fields it declared —
// never add a request/limit a container doesn't have (that would change
// its QoS class), never remove one. Containers without either field are
// skipped entirely.
func mutateResources(containers []corev1.Container, d store.Diff) bool {
	var targets []containerShare
	var totalReq int64
	for i, c := range containers {
		req, lim := fieldValues(d.Resource, c.Resources.Requests, c.Resources.Limits)
		if req == 0 && lim == 0 {
			continue
		}
		targets = append(targets, containerShare{idx: i, hasReq: req != 0, hasLim: lim != 0, curReq: req})
		totalReq += req
	}
	if len(targets) == 0 {
		return false
	}
	for i := range targets {
		if totalReq > 0 {
			targets[i].share = float64(targets[i].curReq) / float64(totalReq)
		} else {
			targets[i].share = 1.0 / float64(len(targets))
		}
	}

	stepReq := d.ProposedReq - d.CurrentReq
	stepLim := d.ProposedLimit - d.CurrentLimit
	reqAllocs := distribute(stepReq, targets)
	limAllocs := distribute(stepLim, targets)

	for i, t := range targets {
		c := &containers[t.idx]
		if t.hasReq {
			setField(c, d.Resource, true, t.curReq+reqAllocs[i])
		}
		if t.hasLim && d.ProposedLimit != d.CurrentLimit {
			setField(c, d.Resource, false, limValue(d.Resource, c.Resources.Limits)+limAllocs[i])
		}
	}
	return true
}

// distribute splits a step across containers by share; the last
// container absorbs rounding so the allocations sum exactly to step.
func distribute(step int64, targets []containerShare) []int64 {
	out := make([]int64, len(targets))
	var sum int64
	for i, t := range targets {
		if i == len(targets)-1 {
			out[i] = step - sum // remainder
			break
		}
		v := int64(math.Round(float64(step) * t.share))
		out[i] = v
		sum += v
	}
	return out
}

func fieldValues(kind string, r, l corev1.ResourceList) (req, lim int64) {
	if kind == "cpu" {
		return r.Cpu().MilliValue(), l.Cpu().MilliValue()
	}
	return r.Memory().Value(), l.Memory().Value()
}

func limValue(kind string, l corev1.ResourceList) int64 {
	if kind == "cpu" {
		return l.Cpu().MilliValue()
	}
	return l.Memory().Value()
}

func setField(c *corev1.Container, kind string, request bool, value int64) {
	var q resource.Quantity
	if kind == "cpu" {
		q = *resource.NewMilliQuantity(value, resource.DecimalSI)
	} else {
		q = *resource.NewQuantity(value, resource.BinarySI)
	}
	name := corev1.ResourceName(kind)
	if request {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = q
	} else {
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[name] = q
	}
}
