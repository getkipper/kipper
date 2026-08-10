package handlers

import "k8s.io/apimachinery/pkg/util/intstr"

func portIntStr(port int32) intstr.IntOrString {
	return intstr.FromInt32(port)
}
