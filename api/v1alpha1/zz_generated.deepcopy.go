package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out.
func (in *GatewayReference) DeepCopyInto(out *GatewayReference) {
	*out = *in
}

// DeepCopy creates a deep copy of GatewayReference.
func (in *GatewayReference) DeepCopy() *GatewayReference {
	if in == nil {
		return nil
	}
	out := new(GatewayReference)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCacheInvalidationSpec) DeepCopyInto(out *VarnishCacheInvalidationSpec) {
	*out = *in
	out.GatewayRef = in.GatewayRef
	if in.TTL != nil {
		in, out := &in.TTL, &out.TTL
		*out = new(metav1.Duration)
		**out = **in
	}
}

// DeepCopy creates a deep copy of VarnishCacheInvalidationSpec.
func (in *VarnishCacheInvalidationSpec) DeepCopy() *VarnishCacheInvalidationSpec {
	if in == nil {
		return nil
	}
	out := new(VarnishCacheInvalidationSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *PodResult) DeepCopyInto(out *PodResult) {
	*out = *in
	in.CompletedAt.DeepCopyInto(&out.CompletedAt)
}

// DeepCopy creates a deep copy of PodResult.
func (in *PodResult) DeepCopy() *PodResult {
	if in == nil {
		return nil
	}
	out := new(PodResult)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCacheInvalidationStatus) DeepCopyInto(out *VarnishCacheInvalidationStatus) {
	*out = *in
	if in.CompletedAt != nil {
		in, out := &in.CompletedAt, &out.CompletedAt
		*out = (*in).DeepCopy()
	}
	if in.PodResults != nil {
		in, out := &in.PodResults, &out.PodResults
		*out = make([]PodResult, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of VarnishCacheInvalidationStatus.
func (in *VarnishCacheInvalidationStatus) DeepCopy() *VarnishCacheInvalidationStatus {
	if in == nil {
		return nil
	}
	out := new(VarnishCacheInvalidationStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCacheInvalidation) DeepCopyInto(out *VarnishCacheInvalidation) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy creates a deep copy of VarnishCacheInvalidation.
func (in *VarnishCacheInvalidation) DeepCopy() *VarnishCacheInvalidation {
	if in == nil {
		return nil
	}
	out := new(VarnishCacheInvalidation)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VarnishCacheInvalidation) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCacheInvalidationList) DeepCopyInto(out *VarnishCacheInvalidationList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]VarnishCacheInvalidation, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of VarnishCacheInvalidationList.
func (in *VarnishCacheInvalidationList) DeepCopy() *VarnishCacheInvalidationList {
	if in == nil {
		return nil
	}
	out := new(VarnishCacheInvalidationList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VarnishCacheInvalidationList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *ConfigMapReference) DeepCopyInto(out *ConfigMapReference) {
	*out = *in
}

// DeepCopy creates a deep copy of ConfigMapReference.
func (in *ConfigMapReference) DeepCopy() *ConfigMapReference {
	if in == nil {
		return nil
	}
	out := new(ConfigMapReference)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishLogging) DeepCopyInto(out *VarnishLogging) {
	*out = *in
	if in.ExtraArgs != nil {
		in, out := &in.ExtraArgs, &out.ExtraArgs
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy creates a deep copy of VarnishLogging.
func (in *VarnishLogging) DeepCopy() *VarnishLogging {
	if in == nil {
		return nil
	}
	out := new(VarnishLogging)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *GatewayClassParametersSpec) DeepCopyInto(out *GatewayClassParametersSpec) {
	*out = *in
	if in.UserVCLConfigMapRef != nil {
		in, out := &in.UserVCLConfigMapRef, &out.UserVCLConfigMapRef
		*out = new(ConfigMapReference)
		**out = **in
	}
	if in.VarnishdExtraArgs != nil {
		in, out := &in.VarnishdExtraArgs, &out.VarnishdExtraArgs
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Logging != nil {
		in, out := &in.Logging, &out.Logging
		*out = new(VarnishLogging)
		(*in).DeepCopyInto(*out)
	}
	if in.ExtraVolumes != nil {
		in, out := &in.ExtraVolumes, &out.ExtraVolumes
		*out = make([]corev1.Volume, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.ExtraVolumeMounts != nil {
		in, out := &in.ExtraVolumeMounts, &out.ExtraVolumeMounts
		*out = make([]corev1.VolumeMount, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.ExtraInitContainers != nil {
		in, out := &in.ExtraInitContainers, &out.ExtraInitContainers
		*out = make([]corev1.Container, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of GatewayClassParametersSpec.
func (in *GatewayClassParametersSpec) DeepCopy() *GatewayClassParametersSpec {
	if in == nil {
		return nil
	}
	out := new(GatewayClassParametersSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *GatewayClassParameters) DeepCopyInto(out *GatewayClassParameters) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy creates a deep copy of GatewayClassParameters.
func (in *GatewayClassParameters) DeepCopy() *GatewayClassParameters {
	if in == nil {
		return nil
	}
	out := new(GatewayClassParameters)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *GatewayClassParameters) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *GatewayClassParametersList) DeepCopyInto(out *GatewayClassParametersList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]GatewayClassParameters, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of GatewayClassParametersList.
func (in *GatewayClassParametersList) DeepCopy() *GatewayClassParametersList {
	if in == nil {
		return nil
	}
	out := new(GatewayClassParametersList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *GatewayClassParametersList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *HeaderBypassCondition) DeepCopyInto(out *HeaderBypassCondition) {
	*out = *in
}

// DeepCopy creates a deep copy of HeaderBypassCondition.
func (in *HeaderBypassCondition) DeepCopy() *HeaderBypassCondition {
	if in == nil {
		return nil
	}
	out := new(HeaderBypassCondition)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *BypassSpec) DeepCopyInto(out *BypassSpec) {
	*out = *in
	if in.Headers != nil {
		in, out := &in.Headers, &out.Headers
		*out = make([]HeaderBypassCondition, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy creates a deep copy of BypassSpec.
func (in *BypassSpec) DeepCopy() *BypassSpec {
	if in == nil {
		return nil
	}
	out := new(BypassSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *QueryParameterKeySpec) DeepCopyInto(out *QueryParameterKeySpec) {
	*out = *in
	if in.Include != nil {
		in, out := &in.Include, &out.Include
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Exclude != nil {
		in, out := &in.Exclude, &out.Exclude
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy creates a deep copy of QueryParameterKeySpec.
func (in *QueryParameterKeySpec) DeepCopy() *QueryParameterKeySpec {
	if in == nil {
		return nil
	}
	out := new(QueryParameterKeySpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *CacheKeySpec) DeepCopyInto(out *CacheKeySpec) {
	*out = *in
	if in.Headers != nil {
		in, out := &in.Headers, &out.Headers
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.QueryParameters != nil {
		in, out := &in.QueryParameters, &out.QueryParameters
		*out = new(QueryParameterKeySpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy creates a deep copy of CacheKeySpec.
func (in *CacheKeySpec) DeepCopy() *CacheKeySpec {
	if in == nil {
		return nil
	}
	out := new(CacheKeySpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *PolicyTargetReference) DeepCopyInto(out *PolicyTargetReference) {
	*out = *in
	if in.SectionName != nil {
		in, out := &in.SectionName, &out.SectionName
		*out = new(string)
		**out = **in
	}
}

// DeepCopy creates a deep copy of PolicyTargetReference.
func (in *PolicyTargetReference) DeepCopy() *PolicyTargetReference {
	if in == nil {
		return nil
	}
	out := new(PolicyTargetReference)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCachePolicyAncestorStatus) DeepCopyInto(out *VarnishCachePolicyAncestorStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of VarnishCachePolicyAncestorStatus.
func (in *VarnishCachePolicyAncestorStatus) DeepCopy() *VarnishCachePolicyAncestorStatus {
	if in == nil {
		return nil
	}
	out := new(VarnishCachePolicyAncestorStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCachePolicySpec) DeepCopyInto(out *VarnishCachePolicySpec) {
	*out = *in
	in.TargetRef.DeepCopyInto(&out.TargetRef)
	if in.DefaultTTL != nil {
		in, out := &in.DefaultTTL, &out.DefaultTTL
		*out = new(metav1.Duration)
		**out = **in
	}
	if in.ForcedTTL != nil {
		in, out := &in.ForcedTTL, &out.ForcedTTL
		*out = new(metav1.Duration)
		**out = **in
	}
	if in.Grace != nil {
		in, out := &in.Grace, &out.Grace
		*out = new(metav1.Duration)
		**out = **in
	}
	if in.Keep != nil {
		in, out := &in.Keep, &out.Keep
		*out = new(metav1.Duration)
		**out = **in
	}
	if in.RequestCoalescing != nil {
		in, out := &in.RequestCoalescing, &out.RequestCoalescing
		*out = new(bool)
		**out = **in
	}
	if in.CacheKey != nil {
		in, out := &in.CacheKey, &out.CacheKey
		*out = new(CacheKeySpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Bypass != nil {
		in, out := &in.Bypass, &out.Bypass
		*out = new(BypassSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy creates a deep copy of VarnishCachePolicySpec.
func (in *VarnishCachePolicySpec) DeepCopy() *VarnishCachePolicySpec {
	if in == nil {
		return nil
	}
	out := new(VarnishCachePolicySpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCachePolicyStatus) DeepCopyInto(out *VarnishCachePolicyStatus) {
	*out = *in
	if in.Ancestors != nil {
		in, out := &in.Ancestors, &out.Ancestors
		*out = make([]VarnishCachePolicyAncestorStatus, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of VarnishCachePolicyStatus.
func (in *VarnishCachePolicyStatus) DeepCopy() *VarnishCachePolicyStatus {
	if in == nil {
		return nil
	}
	out := new(VarnishCachePolicyStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCachePolicy) DeepCopyInto(out *VarnishCachePolicy) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy creates a deep copy of VarnishCachePolicy.
func (in *VarnishCachePolicy) DeepCopy() *VarnishCachePolicy {
	if in == nil {
		return nil
	}
	out := new(VarnishCachePolicy)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VarnishCachePolicy) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *VarnishCachePolicyList) DeepCopyInto(out *VarnishCachePolicyList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]VarnishCachePolicy, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy of VarnishCachePolicyList.
func (in *VarnishCachePolicyList) DeepCopy() *VarnishCachePolicyList {
	if in == nil {
		return nil
	}
	out := new(VarnishCachePolicyList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VarnishCachePolicyList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
