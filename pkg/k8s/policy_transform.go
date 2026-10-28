package k8s

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyinfo "github.com/aws/amazon-network-policy-controller-k8s/api/v1alpha1"
)

// LastChangeTriggerTimeAnnotation is the only PolicyEndpoint annotation kept in the cache.
const LastChangeTriggerTimeAnnotation = "networking.k8s.aws/last-change-trigger-time"

// StripDownNetworkPolicyTransformFunc trims a NetworkPolicy before it enters the informer cache.
// Spec is kept verbatim; managedFields and annotations are dropped since the controller never reads them.
func StripDownNetworkPolicyTransformFunc(obj interface{}) (interface{}, error) {
	if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
		np.ObjectMeta = stripObjectMeta(np.ObjectMeta, nil)
		return np, nil
	}
	return obj, nil
}

// StripDownPolicyEndpointTransformFunc trims a PolicyEndpoint before it enters the informer cache.
// Spec is kept verbatim so EqualPolicyEndpointSpec and the bin-packing see the same data.
func StripDownPolicyEndpointTransformFunc(obj interface{}) (interface{}, error) {
	if pe, ok := obj.(*policyinfo.PolicyEndpoint); ok {
		var annotations map[string]string
		if v, found := pe.Annotations[LastChangeTriggerTimeAnnotation]; found {
			annotations = map[string]string{LastChangeTriggerTimeAnnotation: v}
		}
		pe.ObjectMeta = stripObjectMeta(pe.ObjectMeta, annotations)
		pe.Status = policyinfo.PolicyEndpointStatus{}
		return pe, nil
	}
	return obj, nil
}

// stripObjectMeta keeps the identity, lifecycle and selector metadata the controller and its patches rely on.
func stripObjectMeta(m metav1.ObjectMeta, annotations map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:                       m.Name,
		GenerateName:               m.GenerateName,
		Namespace:                  m.Namespace,
		UID:                        m.UID,
		ResourceVersion:            m.ResourceVersion,
		Generation:                 m.Generation,
		CreationTimestamp:          m.CreationTimestamp,
		DeletionTimestamp:          m.DeletionTimestamp,
		DeletionGracePeriodSeconds: m.DeletionGracePeriodSeconds,
		Labels:                     m.Labels,
		Annotations:                annotations,
		OwnerReferences:            m.OwnerReferences,
		Finalizers:                 m.Finalizers,
	}
}
