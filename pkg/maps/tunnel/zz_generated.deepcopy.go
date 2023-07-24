//go:build !ignore_autogenerated
// +build !ignore_autogenerated

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Code generated by deepcopy-gen. DO NOT EDIT.

package tunnel

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *TunnelIP) DeepCopyInto(out *TunnelIP) {
	*out = *in
	in.IP.DeepCopyInto(&out.IP)
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new TunnelIP.
func (in *TunnelIP) DeepCopy() *TunnelIP {
	if in == nil {
		return nil
	}
	out := new(TunnelIP)
	in.DeepCopyInto(out)
	return out
}