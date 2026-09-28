package scalable

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/caas-team/gokubedownscaler/internal/pkg/definitions"
	"github.com/caas-team/gokubedownscaler/internal/pkg/metrics"
	"github.com/caas-team/gokubedownscaler/internal/pkg/values"
	"github.com/wI2L/jsondiff"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// definedResource wraps an unstructured custom resource described by a workload
// definition. It holds the behavior shared by both shapes; definedReplicaResource
// and definedSuspendResource add the shape-specific methods.
type definedResource struct {
	*unstructured.Unstructured
	def *definitions.Definition
}

// definedReplicaResource is a definedResource scaled by an integer replica field.
type definedReplicaResource struct {
	*definedResource
}

// definedSuspendResource is a definedResource scaled by a suspend field or annotation.
type definedSuspendResource struct {
	*definedResource
}

func definitionGVK(def *definitions.Definition) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: def.Group, Version: def.Version, Kind: def.Kind}
}

// newDefinedWorkload wraps obj in the workload type matching its definition's shape.
//
//nolint:ireturn // returns the shape-specific workload
func newDefinedWorkload(obj *unstructured.Unstructured, def *definitions.Definition) Workload {
	setGroupVersionKindIfEmpty(obj, definitionGVK(def))

	resource := &definedResource{Unstructured: obj, def: def}
	if def.Replicas != nil {
		return &replicaScaledWorkload{&definedReplicaResource{resource}}
	}

	return &suspendScaledWorkload{&definedSuspendResource{resource}}
}

// getDefinedWorkloads lists all resources of a definition in the namespace. A
// CRD that is not installed is skipped with a warning.
func getDefinedWorkloads(def *definitions.Definition, namespace string, clientsets *Clientsets, ctx context.Context) ([]Workload, error) {
	gvk := definitionGVK(def)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))

	if err := clientsets.Client.List(ctx, list, ctrlclient.InNamespace(namespace)); err != nil {
		if apimeta.IsNoMatchError(err) {
			slog.Warn("CRD not found in cluster, skipping", "resource", def.Resource, "kind", gvk.Kind, "error", err)
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get %s: %w", def.Resource, err)
	}

	results := make([]Workload, 0, len(list.Items))
	for i := range list.Items {
		results = append(results, newDefinedWorkload(&list.Items[i], def))
	}

	return results, nil
}

// parseDefinedWorkloadFromBytes parses an admission request object of a defined kind.
//
//nolint:ireturn // returns the shape-specific workload
func parseDefinedWorkloadFromBytes(def *definitions.Definition, rawObject []byte) (Workload, error) {
	var obj unstructured.Unstructured
	if err := json.Unmarshal(rawObject, &obj); err != nil {
		return nil, fmt.Errorf("failed to decode %s: %w", def.Kind, err)
	}

	return newDefinedWorkload(&obj, def), nil
}

// getReplicas gets the current replicas from the definition's replica path.
func (d *definedReplicaResource) getReplicas() (values.Replicas, error) {
	path := d.def.Replicas.Path

	val, found, err := unstructured.NestedFieldNoCopy(d.Object, definitions.SplitPath(path)...)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s for %s %s/%s: %w", path, d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	if !found {
		return nil, newNoReplicasError(d.GetKind(), d.GetName())
	}

	replicas, ok := unstructuredReplicasToInt32(val)
	if !ok {
		return nil, newUnexpectedReplicasTypeError(val, d.GetKind(), d.GetNamespace(), d.GetName())
	}

	return values.AbsoluteReplicas(replicas), nil
}

// minimumReplicas returns the definition's replica floor.
func (d *definedReplicaResource) minimumReplicas() int32 {
	return d.def.Replicas.Minimum
}

// setReplicas sets the replica path. A target below the definition's minimum is
// clamped to it, and a park (target 0) is refused while the park guard fails.
func (d *definedReplicaResource) setReplicas(replicas int32) error {
	target := max(replicas, d.def.Replicas.Minimum)

	if target == 0 && d.def.ParkGuard != nil {
		annotations := d.GetAnnotations()
		for _, required := range d.def.ParkGuard.RequireAnnotations {
			if _, ok := annotations[required]; !ok {
				return newParkRefusedError(d.GetKind(), d.GetNamespace(), d.GetName(),
					fmt.Sprintf("required annotation %q is absent", required))
			}
		}
	}

	path := d.def.Replicas.Path
	if err := unstructured.SetNestedField(d.Object, int64(target), definitions.SplitPath(path)...); err != nil {
		return fmt.Errorf("failed to set %s for %s %s/%s: %w", path, d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	return nil
}

// getSavedResourcesRequests returns the per-pod requests multiplied by the change in replicas.
func (d *definedReplicaResource) getSavedResourcesRequests(diffReplicas int32) *metrics.SavedResources {
	if d.def.Savings == nil {
		return metrics.NewSavedResources(0, 0)
	}

	cpu, memory := podRequests(d.Object, d.def.Savings)

	return metrics.NewSavedResources(cpu*float64(diffReplicas), memory*float64(diffReplicas))
}

// Copy creates a deep copy of the workload.
func (d *definedReplicaResource) Copy() (Workload, error) {
	if d.Object == nil {
		return nil, newNilUnderlyingObjectError(d.GetKind())
	}

	return &replicaScaledWorkload{&definedReplicaResource{d.deepCopy()}}, nil
}

// Compare compares the workload with another workload and returns the differences as a jsondiff.Patch.
func (d *definedReplicaResource) Compare(workloadCopy Workload) (jsondiff.Patch, error) {
	wrapper, isWrapper := workloadCopy.(*replicaScaledWorkload)
	if !isWrapper {
		return nil, newExpectTypeGotTypeError((*replicaScaledWorkload)(nil), workloadCopy)
	}

	other, ok := wrapper.replicaScaledResource.(*definedReplicaResource)
	if !ok {
		return nil, newExpectTypeGotTypeError((*definedReplicaResource)(nil), wrapper.replicaScaledResource)
	}

	return d.compare(other.definedResource)
}

// getSuspend gets the current suspend state and the target downscale state.
//
//nolint:nonamedreturns // named returns document the (current, target) contract of the interface
func (d *definedSuspendResource) getSuspend() (currentValue, targetDownscaleState values.Replicas) {
	current := d.suspendValue() == d.def.Suspend.SuspendedValue

	return values.BooleanReplicas(current), values.BooleanReplicas(true)
}

// suspendValue reads the suspend annotation or field as a string. A missing value reads as "".
func (d *definedSuspendResource) suspendValue() string {
	if d.def.Suspend.Annotation != "" {
		return d.GetAnnotations()[d.def.Suspend.Annotation]
	}

	val, found, err := unstructured.NestedFieldNoCopy(d.Object, definitions.SplitPath(d.def.Suspend.Path)...)
	if err != nil || !found {
		return ""
	}

	switch typed := val.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprint(typed)
	}
}

// setSuspend writes the suspended or resumed value. For a field path, "true" and
// "false" are written as booleans and anything else as a string.
func (d *definedSuspendResource) setSuspend(suspend bool) {
	value := d.def.Suspend.ResumedValue
	if suspend {
		value = d.def.Suspend.SuspendedValue
	}

	if d.def.Suspend.Annotation != "" {
		annotations := d.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}

		annotations[d.def.Suspend.Annotation] = value
		d.SetAnnotations(annotations)

		return
	}

	var fieldValue any = value
	if value == "true" || value == "false" {
		fieldValue = value == "true"
	}

	if err := unstructured.SetNestedField(d.Object, fieldValue, definitions.SplitPath(d.def.Suspend.Path)...); err != nil {
		slog.Error("failed to set suspend field", "path", d.def.Suspend.Path,
			"kind", d.GetKind(), "namespace", d.GetNamespace(), "name", d.GetName(), "error", err)
	}
}

// getSavedResourcesRequests returns the requests of every pod the suspend removes.
func (d *definedSuspendResource) getSavedResourcesRequests() *metrics.SavedResources {
	savings := d.def.Savings
	if savings == nil {
		return metrics.NewSavedResources(0, 0)
	}

	groups := []map[string]any{d.Object}

	if savings.GroupsPath != "" {
		groups = nil

		items, _, _ := unstructured.NestedSlice(d.Object, definitions.SplitPath(savings.GroupsPath)...)
		for _, item := range items {
			if group, ok := item.(map[string]any); ok {
				groups = append(groups, group)
			}
		}
	}

	var totalCPU, totalMemory float64

	for _, group := range groups {
		count := int64(1)

		if savings.CountPath != "" {
			val, _, _ := unstructured.NestedFieldNoCopy(group, definitions.SplitPath(savings.CountPath)...)
			if parsed, ok := unstructuredReplicasToInt32(val); ok && parsed > 0 {
				count = int64(parsed)
			}
		}

		cpu, memory := podRequests(group, savings)
		totalCPU += cpu * float64(count)
		totalMemory += memory * float64(count)
	}

	return metrics.NewSavedResources(totalCPU, totalMemory)
}

// Copy creates a deep copy of the workload.
func (d *definedSuspendResource) Copy() (Workload, error) {
	if d.Object == nil {
		return nil, newNilUnderlyingObjectError(d.GetKind())
	}

	return &suspendScaledWorkload{&definedSuspendResource{d.deepCopy()}}, nil
}

// Compare compares the workload with another workload and returns the differences as a jsondiff.Patch.
func (d *definedSuspendResource) Compare(workloadCopy Workload) (jsondiff.Patch, error) {
	wrapper, isWrapper := workloadCopy.(*suspendScaledWorkload)
	if !isWrapper {
		return nil, newExpectTypeGotTypeError((*suspendScaledWorkload)(nil), workloadCopy)
	}

	other, ok := wrapper.suspendScaledResource.(*definedSuspendResource)
	if !ok {
		return nil, newExpectTypeGotTypeError((*definedSuspendResource)(nil), wrapper.suspendScaledResource)
	}

	return d.compare(other.definedResource)
}

// deepCopy copies the underlying object. Unstructured wraps a map, so a struct
// copy would be shallow and a scale on the copy would leak into the original.
func (d *definedResource) deepCopy() *definedResource {
	return &definedResource{Unstructured: d.DeepCopy(), def: d.def}
}

func (d *definedResource) compare(other *definedResource) (jsondiff.Patch, error) {
	if d.Object == nil || other.Object == nil {
		return nil, newNilUnderlyingObjectError(d.GetKind())
	}

	diff, err := jsondiff.Compare(d.Object, other.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to compare %s: %w", d.GetKind(), err)
	}

	return diff, nil
}

// Reget regets the workload to ensure the latest state.
func (d *definedResource) Reget(clientsets *Clientsets, ctx context.Context) error {
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(definitionGVK(d.def))

	err := clientsets.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: d.GetNamespace(), Name: d.GetName()}, fresh)
	if err != nil {
		return fmt.Errorf("failed to get %s %s/%s: %w", d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	d.Unstructured = fresh

	return nil
}

// Update updates the resource with all changes made to it.
func (d *definedResource) Update(clientsets *Clientsets, ctx context.Context) error {
	err := clientsets.Client.Update(ctx, d.Unstructured)
	if err != nil {
		return fmt.Errorf("failed to update %s %s/%s: %w", d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	return nil
}

// GetChildren returns the workloads matched by the definition's children, wrapped
// as replica-scaled workloads so they are scaled to 0 with the parent.
func (d *definedResource) GetChildren(ctx context.Context, clientsets *Clientsets) ([]Workload, error) {
	var results []Workload

	for _, child := range d.def.Children {
		children, err := d.getChildren(ctx, clientsets, child)
		if err != nil {
			return nil, err
		}

		results = append(results, children...)
	}

	return results, nil
}

func (d *definedResource) getChildren(ctx context.Context, clientsets *Clientsets, child definitions.Child) ([]Workload, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(child.Kind + "List"))

	options := []ctrlclient.ListOption{ctrlclient.InNamespace(d.GetNamespace())}
	if child.Match == definitions.MatchLabelEqualsName {
		options = append(options, ctrlclient.MatchingLabels{child.Label: d.GetName()})
	}

	if err := clientsets.Client.List(ctx, list, options...); err != nil {
		return nil, fmt.Errorf("failed to list %s children of %s %s/%s: %w", child.Kind, d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	results := make([]Workload, 0, len(list.Items))

	for i := range list.Items {
		item := &list.Items[i]
		if child.Match == definitions.MatchOwnerReference && !d.owns(item) {
			continue
		}

		workload, err := toChildWorkload(item, child.Kind)
		if err != nil {
			return nil, err
		}

		results = append(results, workload)
	}

	return results, nil
}

// owns reports whether obj has an ownerReference to this resource's group, kind and name.
func (d *definedResource) owns(obj *unstructured.Unstructured) bool {
	for _, owner := range obj.GetOwnerReferences() {
		groupVersion, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil {
			continue
		}

		if groupVersion.Group == d.def.Group && owner.Kind == d.def.Kind && owner.Name == d.GetName() {
			return true
		}
	}

	return false
}

// toChildWorkload converts a listed child into the typed built-in workload.
//
//nolint:ireturn // returns the kind-specific workload
func toChildWorkload(obj *unstructured.Unstructured, kind string) (Workload, error) {
	switch kind {
	case statefulSetKind:
		sts := &appsv1.StatefulSet{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, sts); err != nil {
			return nil, fmt.Errorf("failed to convert statefulset %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}

		setGroupVersionKindIfEmpty(sts, appsv1.SchemeGroupVersion.WithKind(statefulSetKind))

		return &replicaScaledWorkload{&statefulSet{sts}}, nil
	case deploymentKind:
		dep := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, dep); err != nil {
			return nil, fmt.Errorf("failed to convert deployment %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}

		setGroupVersionKindIfEmpty(dep, appsv1.SchemeGroupVersion.WithKind(deploymentKind))

		return &replicaScaledWorkload{&deployment{dep}}, nil
	default:
		return nil, newInvalidResourceError(kind)
	}
}

// podRequests returns the CPU and memory requested by one pod, read from obj per
// savings. Missing or unparseable values count as zero so accounting never breaks a scale.
//
//nolint:nonamedreturns // named returns tell the two float64 results apart
func podRequests(obj map[string]any, savings *definitions.Savings) (cpu, memory float64) {
	if savings.RequestsPath != "" {
		return requestsAt(obj, definitions.SplitPath(savings.RequestsPath))
	}

	containers, _, _ := unstructured.NestedSlice(obj, definitions.SplitPath(savings.ContainersPath)...)
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			continue
		}

		containerCPU, containerMemory := requestsAt(container, []string{"resources", "requests"})
		cpu += containerCPU
		memory += containerMemory
	}

	return cpu, memory
}

//nolint:nonamedreturns // named returns tell the two float64 results apart
func requestsAt(obj map[string]any, path []string) (cpu, memory float64) {
	cpuQuantity, _, _ := unstructured.NestedString(obj, append(append([]string{}, path...), "cpu")...)
	memoryQuantity, _, _ := unstructured.NestedString(obj, append(append([]string{}, path...), "memory")...)

	return parseQuantityAsFloat(&cpuQuantity), parseQuantityAsFloat(&memoryQuantity)
}
