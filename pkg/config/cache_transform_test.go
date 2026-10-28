package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/amazon-network-policy-controller-k8s/api/v1alpha1"
	"github.com/aws/amazon-network-policy-controller-k8s/pkg/k8s"
)

func cacheTestMeta(name, kind string) metav1.ObjectMeta {
	ctrl := true
	return metav1.ObjectMeta{
		Name: name, Namespace: "ns", UID: "uid-" + "1", ResourceVersion: "9", Generation: 3,
		Labels:      map[string]string{"app": "web"},
		Annotations: map[string]string{k8s.LastChangeTriggerTimeAnnotation: "t0", "kubectl.kubernetes.io/last-applied-configuration": "{}"},
		Finalizers:  []string{"networking.k8s.aws/resources"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "networking.k8s.io/v1", Kind: kind, Name: "np",
			UID: "owner", Controller: &ctrl}},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)}}},
	}
}

func fakeAPIServer(t *testing.T, items map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entry, ok := items[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		obj := entry.(client.Object)
		gvk := obj.GetObjectKind().GroupVersionKind()
		w.Header().Set("Content-Type", "application/json")
		q := req.URL.Query()
		if q.Get("watch") != "true" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"kind": gvk.Kind + "List", "apiVersion": gvk.GroupVersion().String(),
				"metadata": map[string]string{"resourceVersion": "10"}, "items": []client.Object{obj}})
			return
		}
		enc := json.NewEncoder(w)
		if q.Get("sendInitialEvents") == "true" {
			_ = enc.Encode(map[string]interface{}{"type": "ADDED", "object": obj})
			_ = enc.Encode(map[string]interface{}{"type": "BOOKMARK", "object": map[string]interface{}{
				"kind": gvk.Kind, "apiVersion": gvk.GroupVersion().String(),
				"metadata": map[string]interface{}{"resourceVersion": "10",
					"annotations": map[string]string{metav1.InitialEventsAnnotationKey: "true"}}}})
		}
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
}

// Test_BuildCacheOptions_informerHoldsStrippedObjects runs a real controller-runtime cache built from
// BuildCacheOptions and checks Get, the policyRef field index and label selection all work on trimmed objects.
func Test_BuildCacheOptions_informerHoldsStrippedObjects(t *testing.T) {
	np := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: cacheTestMeta("np", "NetworkPolicy"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}}}}},
		},
	}
	np.OwnerReferences = nil
	pe := &v1alpha1.PolicyEndpoint{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.aws/v1alpha1", Kind: "PolicyEndpoint"},
		ObjectMeta: cacheTestMeta("np-abcde", "NetworkPolicy"),
		Spec: v1alpha1.PolicyEndpointSpec{
			PodSelector:          &np.Spec.PodSelector,
			PolicyRef:            v1alpha1.PolicyReference{Namespace: "ns", Name: "np"},
			PodIsolation:         np.Spec.PolicyTypes,
			Ingress:              []v1alpha1.EndpointInfo{{CIDR: "10.0.0.0/8"}},
			PodSelectorEndpoints: []v1alpha1.PodEndpoint{{HostIP: "1.1.1.1", PodIP: "10.0.0.5", Name: "p", Namespace: "ns"}},
		},
	}
	srv := fakeAPIServer(t, map[string]interface{}{
		"/apis/networking.k8s.io/v1/networkpolicies":        np,
		"/apis/networking.k8s.aws/v1alpha1/policyendpoints": pe,
	})
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"), meta.RESTScopeNamespace)
	for _, kind := range []string{"PolicyEndpoint", "ApplicationNetworkPolicy"} {
		mapper.Add(v1alpha1.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"ClusterNetworkPolicy", "ClusterPolicyEndpoint"} {
		mapper.Add(v1alpha1.GroupVersion.WithKind(kind), meta.RESTScopeRoot)
	}
	for _, kind := range []string{"Pod", "Service"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)

	opts := BuildCacheOptions()
	opts.Scheme, opts.Mapper = scheme, mapper
	c, err := cache.New(&rest.Config{Host: srv.URL}, opts)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, c.IndexField(ctx, &v1alpha1.PolicyEndpoint{}, "spec.policyRef.name", func(o client.Object) []string {
		return []string{o.(*v1alpha1.PolicyEndpoint).Spec.PolicyRef.Name}
	}))
	_, err = c.GetInformer(ctx, &networkingv1.NetworkPolicy{})
	require.NoError(t, err)
	go func() { _ = c.Start(ctx) }()
	require.True(t, c.WaitForCacheSync(ctx))

	gotNP := &networkingv1.NetworkPolicy{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(np), gotNP))
	assert.Equal(t, np.Spec, gotNP.Spec)
	assert.Nil(t, gotNP.ManagedFields)
	assert.Nil(t, gotNP.Annotations)
	assert.Equal(t, np.Finalizers, gotNP.Finalizers)
	assert.Equal(t, np.ResourceVersion, gotNP.ResourceVersion)

	peList := &v1alpha1.PolicyEndpointList{}
	require.NoError(t, c.List(ctx, peList, client.InNamespace("ns"), client.MatchingFields{"spec.policyRef.name": "np"}))
	require.Len(t, peList.Items, 1)
	got := peList.Items[0]
	assert.Equal(t, pe.Spec, got.Spec)
	assert.Nil(t, got.ManagedFields)
	assert.Equal(t, map[string]string{k8s.LastChangeTriggerTimeAnnotation: "t0"}, got.Annotations)
	assert.Equal(t, pe.OwnerReferences, got.OwnerReferences)
	assert.Equal(t, pe.Labels, got.Labels)

	byLabel := &v1alpha1.PolicyEndpointList{}
	require.NoError(t, c.List(ctx, byLabel, client.InNamespace("ns"), client.MatchingLabels{"app": "web"}))
	assert.Len(t, byLabel.Items, 1)
}
