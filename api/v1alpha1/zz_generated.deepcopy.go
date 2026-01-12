package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

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
func (in *GatewayClassParametersSpec) DeepCopyInto(out *GatewayClassParametersSpec) {
	*out = *in
	if in.UserVCLConfigMapRef != nil {
		in, out := &in.UserVCLConfigMapRef, &out.UserVCLConfigMapRef
		*out = new(ConfigMapReference)
		**out = **in
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
