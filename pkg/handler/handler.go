package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	wzrdtalesscpv1alpha1 "github.com/WizardTales/podpreset-webhook-ng/api/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	annotationPrefix = "podpreset.admission.kubernetes.io"
)

// +kubebuilder:webhook:path=/mutate,mutating=true,failurePolicy=ignore,groups="",resources=pods,verbs=create,versions=v1,name=mpod.wzrdtalesscp.wzrdtales.com,sideEffects=None,admissionReviewVersions={v1,v1beta1}
// +kubebuilder:rbac:groups=wzrdtalesscp.wzrdtales.com,resources=podpresets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;update;patch

// PodPresetMutator mutates Pods
type PodPresetMutator struct {
	Client  client.Client
	decoder *admission.Decoder
	Log     logr.Logger
}

// PodPresetMutator adds an annotation to every incoming pods.
func (a *PodPresetMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := a.Log.WithValues("podpreset-webhook", fmt.Sprintf("%s/%s", req.Namespace, req.Name))

	// Ignore all calls to subresources or resources other than pods.
	// Ignore all operations other than CREATE.
	if len(req.SubResource) != 0 || req.Resource.Group != "" || req.Operation != "CREATE" {
		return admission.Allowed("")
	}

	pod := &corev1.Pod{}

	err := a.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Begin Mutation

	if _, isMirrorPod := pod.Annotations[corev1.MirrorPodAnnotationKey]; isMirrorPod {
		return admission.Allowed("Mirror Pod")
	}

	// Ignore if exclusion annotation is present
	if podAnnotations := pod.GetAnnotations(); podAnnotations != nil {
		if podAnnotations[corev1.PodPresetOptOutAnnotationKey] == "true" {
			return admission.Allowed("Exclusion Annotation Present")
		}
	}

	podPresetList := &wzrdtalesscpv1alpha1.PodPresetList{}
	clusterPodPresetList := &wzrdtalesscpv1alpha1.ClusterPodPresetList{}

	err = a.Client.List(context.TODO(), podPresetList, &client.ListOptions{Namespace: req.Namespace})

	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("Error retrieving ist of PodPresets: %v", err))
	}

	err = a.Client.List(context.TODO(), clusterPodPresetList, &client.ListOptions{})

	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("Error retrieving ist of ClusterPodPresets: %v", err))
	}

	matchingPPs, err := filterPodPresets(*podPresetList, pod)
	matchingCPPs, err := filterClusterPodPresets(*clusterPodPresetList, pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("filtering pod presets failed: %v", err))
	}

	if len(matchingPPs) == 0 && len(matchingCPPs) == 0 {
		return admission.Allowed("")
	}

	presetNames := make([]string, len(matchingPPs))
	for i, pp := range matchingPPs {
		presetNames[i] = pp.GetName()
	}

	clusterPresetNames := make([]string, len(matchingCPPs))
	for i, pp := range matchingCPPs {
		clusterPresetNames[i] = pp.GetName()
	}

	// detect merge conflict
	err = safeToApplyPodPresetsOnPod(pod, matchingPPs)
	if err != nil {
		// conflict, ignore the error, but raise an event
		logger.Info("conflict occurred while applying podpresets: %s on pod: %v err: %v",
			strings.Join(presetNames, ","), pod.GetGenerateName(), err)
		admission.Allowed("")
	}

	// detect merge conflict
	err = safeToApplyClusterPodPresetsOnPod(pod, matchingCPPs)
	if err != nil {
		// conflict, ignore the error, but raise an event
		logger.Info("conflict occurred while applying clusterpodpresets: %s on pod: %v err: %v",
			strings.Join(presetNames, ","), pod.GetGenerateName(), err)
		admission.Allowed("")
	}

	applyPodPresetsOnPod(pod, matchingPPs)
	applyClusterPodPresetsOnPod(pod, matchingCPPs)

	// End Mutation
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// PodPresetMutator implements admission.DecoderInjector.
// A decoder will be automatically injected.

// InjectDecoder injects the decoder.
func (a *PodPresetMutator) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}

// filterPodPresets returns list of PodPresets which match given Pod.
func filterPodPresets(list wzrdtalesscpv1alpha1.PodPresetList, pod *corev1.Pod) ([]*wzrdtalesscpv1alpha1.PodPreset, error) {
	var matchingPPs []*wzrdtalesscpv1alpha1.PodPreset

	for _, pp := range list.Items {
		selector, err := metav1.LabelSelectorAsSelector(&pp.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("label selector conversion failed: %v for selector: %v", pp.Spec.Selector, err)
		}

		// check if the pod labels match the selector
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		matchingPPs = append(matchingPPs, &pp)
	}
	return matchingPPs, nil
}

// filterPodPresets returns list of PodPresets which match given Pod.
func filterClusterPodPresets(list wzrdtalesscpv1alpha1.ClusterPodPresetList, pod *corev1.Pod) ([]*wzrdtalesscpv1alpha1.ClusterPodPreset, error) {
	var matchingPPs []*wzrdtalesscpv1alpha1.ClusterPodPreset

	for _, pp := range list.Items {
		selector, err := metav1.LabelSelectorAsSelector(&pp.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("label selector conversion failed: %v for selector: %v", pp.Spec.Selector, err)
		}

		// check if the pod labels match the selector
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		matchingPPs = append(matchingPPs, &pp)
	}
	return matchingPPs, nil
}

// safeToApplyPodPresetsOnPod determines if there is any conflict in information
// injected by given PodPresets in the Pod.
func safeToApplyPodPresetsOnPod(pod *corev1.Pod, podPresets []*wzrdtalesscpv1alpha1.PodPreset) error {
	var errs []error

	// volumes attribute is defined at the Pod level, so determine if volumes
	// injection is causing any conflict.
	if _, err := mergeVolumes(pod.Spec.Volumes, podPresets); err != nil {
		errs = append(errs, err)
	}

	return utilerrors.NewAggregate(errs)
}

// safeToApplyPodPresetsOnPod determines if there is any conflict in information
// injected by given PodPresets in the Pod.
func safeToApplyClusterPodPresetsOnPod(pod *corev1.Pod, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) error {
	var errs []error

	// volumes attribute is defined at the Pod level, so determine if volumes
	// injection is causing any conflict.
	if _, err := mergeClusterVolumes(pod.Spec.Volumes, podPresets); err != nil {
		errs = append(errs, err)
	}

	return utilerrors.NewAggregate(errs)
}

// safeToApplyPodPresetsOnContainer determines if there is any conflict in
// information injected by given PodPresets in the given container.
func safeToApplyPodPresetsOnContainer(ctr *corev1.Container, podPresets []*wzrdtalesscpv1alpha1.PodPreset) error {
	var errs []error
	// check if it is safe to merge env vars and volume mounts from given podpresets and
	// container's existing env vars.
	if _, err := mergeEnv(ctr.Env, podPresets); err != nil {
		errs = append(errs, err)
	}
	if _, err := mergeVolumeMounts(ctr.VolumeMounts, podPresets); err != nil {
		errs = append(errs, err)
	}

	return utilerrors.NewAggregate(errs)
}

// mergeClusterEnv merges a list of env vars with the env vars injected by given list podPresets.
// It returns an error if it detects any conflict during the merge.
func mergeClusterEnv(envVars []corev1.EnvVar, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) ([]corev1.EnvVar, error) {
	origEnv := map[string]corev1.EnvVar{}
	for _, v := range envVars {
		origEnv[v.Name] = v
	}

	mergedEnv := make([]corev1.EnvVar, len(envVars))
	copy(mergedEnv, envVars)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.Env {

			found, ok := origEnv[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origEnv[v.Name] = v
				mergedEnv = append(mergedEnv, v)
				continue
			}

			// make sure they are identical or throw an error
			if !reflect.DeepEqual(found, v) {
				errs = append(errs, fmt.Errorf("merging env for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedEnv, err
}

// mergeEnv merges a list of env vars with the env vars injected by given list podPresets.
// It returns an error if it detects any conflict during the merge.
func mergeEnv(envVars []corev1.EnvVar, podPresets []*wzrdtalesscpv1alpha1.PodPreset) ([]corev1.EnvVar, error) {
	origEnv := map[string]corev1.EnvVar{}
	for _, v := range envVars {
		origEnv[v.Name] = v
	}

	mergedEnv := make([]corev1.EnvVar, len(envVars))
	copy(mergedEnv, envVars)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.Env {

			found, ok := origEnv[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origEnv[v.Name] = v
				mergedEnv = append(mergedEnv, v)
				continue
			}

			// make sure they are identical or throw an error
			if !reflect.DeepEqual(found, v) {
				errs = append(errs, fmt.Errorf("merging env for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedEnv, err
}

type envFromMergeKey struct {
	prefix           string
	configMapRefName string
	secretRefName    string
}

func newEnvFromMergeKey(e corev1.EnvFromSource) envFromMergeKey {
	k := envFromMergeKey{prefix: e.Prefix}
	if e.ConfigMapRef != nil {
		k.configMapRefName = e.ConfigMapRef.Name
	}
	if e.SecretRef != nil {
		k.secretRefName = e.SecretRef.Name
	}
	return k
}

func mergeClusterEnvFrom(envSources []corev1.EnvFromSource, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) ([]corev1.EnvFromSource, error) {
	var mergedEnvFrom []corev1.EnvFromSource

	// merge envFrom using a identify key to ensure Admit reinvocations are idempotent
	origEnvSources := map[envFromMergeKey]corev1.EnvFromSource{}
	for _, envSource := range envSources {
		origEnvSources[newEnvFromMergeKey(envSource)] = envSource
	}
	mergedEnvFrom = append(mergedEnvFrom, envSources...)
	var errs []error
	for _, pp := range podPresets {
		for _, envFromSource := range pp.Spec.EnvFrom {

			found, ok := origEnvSources[newEnvFromMergeKey(envFromSource)]
			if !ok {
				mergedEnvFrom = append(mergedEnvFrom, envFromSource)
				continue
			}
			if !reflect.DeepEqual(found, envFromSource) {
				errs = append(errs, fmt.Errorf("merging envFrom for %s has a conflict: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), envFromSource, found))
			}
		}

	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedEnvFrom, nil
}

func mergeEnvFrom(envSources []corev1.EnvFromSource, podPresets []*wzrdtalesscpv1alpha1.PodPreset) ([]corev1.EnvFromSource, error) {
	var mergedEnvFrom []corev1.EnvFromSource

	// merge envFrom using a identify key to ensure Admit reinvocations are idempotent
	origEnvSources := map[envFromMergeKey]corev1.EnvFromSource{}
	for _, envSource := range envSources {
		origEnvSources[newEnvFromMergeKey(envSource)] = envSource
	}
	mergedEnvFrom = append(mergedEnvFrom, envSources...)
	var errs []error
	for _, pp := range podPresets {
		for _, envFromSource := range pp.Spec.EnvFrom {

			found, ok := origEnvSources[newEnvFromMergeKey(envFromSource)]
			if !ok {
				mergedEnvFrom = append(mergedEnvFrom, envFromSource)
				continue
			}
			if !reflect.DeepEqual(found, envFromSource) {
				errs = append(errs, fmt.Errorf("merging envFrom for %s has a conflict: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), envFromSource, found))
			}
		}

	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedEnvFrom, nil
}

// mergeClusterVolumeMounts merges given list of VolumeMounts with the volumeMounts
// injected by given podPresets. It returns an error if it detects any conflict during the merge.
func mergeClusterVolumeMounts(volumeMounts []corev1.VolumeMount, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) ([]corev1.VolumeMount, error) {

	origVolumeMounts := map[string]corev1.VolumeMount{}
	volumeMountsByPath := map[string]corev1.VolumeMount{}
	for _, v := range volumeMounts {
		origVolumeMounts[v.Name] = v
		volumeMountsByPath[v.MountPath] = v
	}

	mergedVolumeMounts := make([]corev1.VolumeMount, len(volumeMounts))
	copy(mergedVolumeMounts, volumeMounts)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.VolumeMounts {

			found, ok := origVolumeMounts[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origVolumeMounts[v.Name] = v
				mergedVolumeMounts = append(mergedVolumeMounts, v)
			} else {
				// make sure they are identical or throw an error
				// shall we throw an error for identical volumeMounts ?
				if !reflect.DeepEqual(found, v) {
					errs = append(errs, fmt.Errorf("merging volume mounts for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
				}
			}

			found, ok = volumeMountsByPath[v.MountPath]
			if !ok {
				// if we don't already have it append it and continue
				volumeMountsByPath[v.MountPath] = v
			} else {
				// make sure they are identical or throw an error
				if !reflect.DeepEqual(found, v) {
					errs = append(errs, fmt.Errorf("merging volume mounts for %s has a conflict on mount path %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.MountPath, v, found))
				}
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedVolumeMounts, err
}

// mergeVolumeMounts merges given list of VolumeMounts with the volumeMounts
// injected by given podPresets. It returns an error if it detects any conflict during the merge.
func mergeVolumeMounts(volumeMounts []corev1.VolumeMount, podPresets []*wzrdtalesscpv1alpha1.PodPreset) ([]corev1.VolumeMount, error) {

	origVolumeMounts := map[string]corev1.VolumeMount{}
	volumeMountsByPath := map[string]corev1.VolumeMount{}
	for _, v := range volumeMounts {
		origVolumeMounts[v.Name] = v
		volumeMountsByPath[v.MountPath] = v
	}

	mergedVolumeMounts := make([]corev1.VolumeMount, len(volumeMounts))
	copy(mergedVolumeMounts, volumeMounts)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.VolumeMounts {

			found, ok := origVolumeMounts[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origVolumeMounts[v.Name] = v
				mergedVolumeMounts = append(mergedVolumeMounts, v)
			} else {
				// make sure they are identical or throw an error
				// shall we throw an error for identical volumeMounts ?
				if !reflect.DeepEqual(found, v) {
					errs = append(errs, fmt.Errorf("merging volume mounts for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
				}
			}

			found, ok = volumeMountsByPath[v.MountPath]
			if !ok {
				// if we don't already have it append it and continue
				volumeMountsByPath[v.MountPath] = v
			} else {
				// make sure they are identical or throw an error
				if !reflect.DeepEqual(found, v) {
					errs = append(errs, fmt.Errorf("merging volume mounts for %s has a conflict on mount path %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.MountPath, v, found))
				}
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	return mergedVolumeMounts, err
}

// mergeVolumes merges given list of Volumes with the volumes injected by given
// podPresets. It returns an error if it detects any conflict during the merge.
func mergeVolumes(volumes []corev1.Volume, podPresets []*wzrdtalesscpv1alpha1.PodPreset) ([]corev1.Volume, error) {
	origVolumes := map[string]corev1.Volume{}
	for _, v := range volumes {
		origVolumes[v.Name] = v
	}

	mergedVolumes := make([]corev1.Volume, len(volumes))
	copy(mergedVolumes, volumes)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.Volumes {

			found, ok := origVolumes[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origVolumes[v.Name] = v
				mergedVolumes = append(mergedVolumes, v)
				continue
			}

			// make sure they are identical or throw an error
			if !reflect.DeepEqual(found, v) {
				errs = append(errs, fmt.Errorf("merging volumes for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	if len(mergedVolumes) == 0 {
		return nil, nil
	}

	return mergedVolumes, err
}

// mergeClusterVolumes merges given list of Volumes with the volumes injected by given
// podPresets. It returns an error if it detects any conflict during the merge.
func mergeClusterVolumes(volumes []corev1.Volume, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) ([]corev1.Volume, error) {
	origVolumes := map[string]corev1.Volume{}
	for _, v := range volumes {
		origVolumes[v.Name] = v
	}

	mergedVolumes := make([]corev1.Volume, len(volumes))
	copy(mergedVolumes, volumes)

	var errs []error

	for _, pp := range podPresets {
		for _, v := range pp.Spec.Volumes {

			found, ok := origVolumes[v.Name]
			if !ok {
				// if we don't already have it append it and continue
				origVolumes[v.Name] = v
				mergedVolumes = append(mergedVolumes, v)
				continue
			}

			// make sure they are identical or throw an error
			if !reflect.DeepEqual(found, v) {
				errs = append(errs, fmt.Errorf("merging volumes for %s has a conflict on %s: \n%#v\ndoes not match\n%#v\n in container", pp.GetName(), v.Name, v, found))
			}
		}
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		return nil, err
	}

	if len(mergedVolumes) == 0 {
		return nil, nil
	}

	return mergedVolumes, err
}

// applyClusterPodPresetsOnPod updates the PodSpec with merged information from all the
// applicable PodPresets. It ignores the errors of merge functions because merge
// errors have already been checked in safeToApplyPodPresetsOnPod function.
func applyClusterPodPresetsOnPod(pod *corev1.Pod, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) {
	if len(podPresets) == 0 {
		return
	}

	volumes, _ := mergeClusterVolumes(pod.Spec.Volumes, podPresets)
	pod.Spec.Volumes = volumes

	for i, ctr := range pod.Spec.Containers {
		applyClusterPodPresetsOnContainer(&ctr, podPresets)
		pod.Spec.Containers[i] = ctr
	}
	for i, iCtr := range pod.Spec.InitContainers {
		applyClusterPodPresetsOnContainer(&iCtr, podPresets)
		pod.Spec.InitContainers[i] = iCtr
	}

	// add annotation
	if pod.ObjectMeta.Annotations == nil {
		pod.ObjectMeta.Annotations = map[string]string{}
	}

	for _, pp := range podPresets {
		pod.ObjectMeta.Annotations[fmt.Sprintf("%s/podpreset-%s", annotationPrefix, pp.GetName())] = pp.GetResourceVersion()
	}
}

// applyPodPresetsOnPod updates the PodSpec with merged information from all the
// applicable PodPresets. It ignores the errors of merge functions because merge
// errors have already been checked in safeToApplyPodPresetsOnPod function.
func applyPodPresetsOnPod(pod *corev1.Pod, podPresets []*wzrdtalesscpv1alpha1.PodPreset) {
	if len(podPresets) == 0 {
		return
	}

	volumes, _ := mergeVolumes(pod.Spec.Volumes, podPresets)
	pod.Spec.Volumes = volumes

	for i, ctr := range pod.Spec.Containers {
		applyPodPresetsOnContainer(&ctr, podPresets)
		pod.Spec.Containers[i] = ctr
	}
	for i, iCtr := range pod.Spec.InitContainers {
		applyPodPresetsOnContainer(&iCtr, podPresets)
		pod.Spec.InitContainers[i] = iCtr
	}

	// add annotation
	if pod.ObjectMeta.Annotations == nil {
		pod.ObjectMeta.Annotations = map[string]string{}
	}

	for _, pp := range podPresets {
		pod.ObjectMeta.Annotations[fmt.Sprintf("%s/podpreset-%s", annotationPrefix, pp.GetName())] = pp.GetResourceVersion()
	}
}

// applyClusterPodPresetsOnContainer injects envVars, VolumeMounts and envFrom from
// given podPresets in to the given container. It ignores conflict errors
// because it assumes those have been checked already by the caller.
func applyClusterPodPresetsOnContainer(ctr *corev1.Container, podPresets []*wzrdtalesscpv1alpha1.ClusterPodPreset) {
	envVars, _ := mergeClusterEnv(ctr.Env, podPresets)
	ctr.Env = envVars

	volumeMounts, _ := mergeClusterVolumeMounts(ctr.VolumeMounts, podPresets)
	ctr.VolumeMounts = volumeMounts

	envFrom, _ := mergeClusterEnvFrom(ctr.EnvFrom, podPresets)
	ctr.EnvFrom = envFrom
}

// applyPodPresetsOnContainer injects envVars, VolumeMounts and envFrom from
// given podPresets in to the given container. It ignores conflict errors
// because it assumes those have been checked already by the caller.
func applyPodPresetsOnContainer(ctr *corev1.Container, podPresets []*wzrdtalesscpv1alpha1.PodPreset) {
	envVars, _ := mergeEnv(ctr.Env, podPresets)
	ctr.Env = envVars

	volumeMounts, _ := mergeVolumeMounts(ctr.VolumeMounts, podPresets)
	ctr.VolumeMounts = volumeMounts

	envFrom, _ := mergeEnvFrom(ctr.EnvFrom, podPresets)
	ctr.EnvFrom = envFrom
}
