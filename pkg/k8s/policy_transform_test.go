package k8s

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	policyinfo "github.com/aws/amazon-network-policy-controller-k8s/api/v1alpha1"
	policyequality "github.com/aws/amazon-network-policy-controller-k8s/pkg/equality"
)

func fullMeta(name string, annotations map[string]string, managers ...string) metav1.ObjectMeta {
	grace := int64(30)
	del := metav1.NewTime(time.Unix(1700000100, 0))
	ctrl := true
	mf := make([]metav1.ManagedFieldsEntry, 0, len(managers))
	for _, m := range managers {
		mf = append(mf, metav1.ManagedFieldsEntry{
			Manager:    m,
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "networking.k8s.io/v1",
			Time:       &del,
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:annotations":{".":{},` +
				`"f:kubectl.kubernetes.io/last-applied-configuration":{}},"f:finalizers":{".":{},"v:\"networking.k8s.aws/resources\"":{}}},` +
				`"f:spec":{"f:egress":{},"f:ingress":{},"f:podSelector":{},"f:policyTypes":{}}}`)},
		})
	}
	return metav1.ObjectMeta{
		Name:                       name,
		GenerateName:               name + "-",
		Namespace:                  "ns-a",
		UID:                        types.UID("11111111-2222-3333-4444-" + fmt.Sprintf("%012d", len(name))),
		ResourceVersion:            "4242",
		Generation:                 7,
		CreationTimestamp:          metav1.NewTime(time.Unix(1700000000, 0)),
		DeletionTimestamp:          &del,
		DeletionGracePeriodSeconds: &grace,
		Labels:                     map[string]string{"app": "web", "team": "a"},
		Annotations:                annotations,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Name: name,
			UID: "owner-uid", Controller: &ctrl, BlockOwnerDeletion: &ctrl,
		}},
		Finalizers:    []string{"networking.k8s.aws/resources"},
		ManagedFields: mf,
	}
}

func sampleNPSpec() networkingv1.NetworkPolicySpec {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(8080)
	end := int32(8090)
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels:      map[string]string{"app": "web"},
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"fe", "be"}}},
		},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port, EndPort: &end}},
			From: []networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}},
				{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}},
				{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/16", Except: []string{"10.0.1.0/24"}}},
			},
		}},
		Egress: []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
		}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
	}
}

func lastApplied(spec networkingv1.NetworkPolicySpec, name string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]string{"name": name, "namespace": "ns-a"}, "spec": spec,
	})
	return string(b)
}

func sampleNP(name string) *networkingv1.NetworkPolicy {
	spec := sampleNPSpec()
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: fullMeta(name, map[string]string{
			"kubectl.kubernetes.io/last-applied-configuration": lastApplied(spec, name),
			"owner": "team-a",
		}, "kubectl-client-side-apply", "controller"),
		Spec: spec,
	}
}

func samplePE(name string, ingress, pods int) *policyinfo.PolicyEndpoint {
	tcp := corev1.ProtocolTCP
	p80 := int32(80)
	pe := &policyinfo.PolicyEndpoint{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.aws/v1alpha1", Kind: "PolicyEndpoint"},
		ObjectMeta: fullMeta(name, map[string]string{
			LastChangeTriggerTimeAnnotation: "2026-10-28T07:00:00.123456789Z",
			"other":                         "value",
		}, "controller"),
		Spec: policyinfo.PolicyEndpointSpec{
			PodSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyRef:    policyinfo.PolicyReference{Namespace: "ns-a", Name: name},
			PodIsolation: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:       []policyinfo.EndpointInfo{{CIDR: "0.0.0.0/0"}, {DomainName: "example.com"}},
		},
	}
	for i := 0; i < ingress; i++ {
		pe.Spec.Ingress = append(pe.Spec.Ingress, policyinfo.EndpointInfo{
			CIDR:   policyinfo.NetworkAddress(fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)),
			Except: []policyinfo.NetworkAddress{"10.255.0.1/32"},
			Ports:  []policyinfo.Port{{Protocol: &tcp, Port: &p80}},
		})
	}
	for i := 0; i < pods; i++ {
		pe.Spec.PodSelectorEndpoints = append(pe.Spec.PodSelectorEndpoints, policyinfo.PodEndpoint{
			HostIP: "192.168.0.1", PodIP: policyinfo.NetworkAddress(fmt.Sprintf("10.1.%d.%d", i/256, i%256)),
			Name: fmt.Sprintf("pod-%d", i), Namespace: "ns-a",
		})
	}
	return pe
}

// assertKeptMeta checks every metadata field the controller reads or sends in a patch survives.
func assertKeptMeta(t *testing.T, want, got metav1.ObjectMeta) {
	t.Helper()
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.GenerateName, got.GenerateName)
	assert.Equal(t, want.Namespace, got.Namespace)
	assert.Equal(t, want.UID, got.UID)
	assert.Equal(t, want.ResourceVersion, got.ResourceVersion)
	assert.Equal(t, want.Generation, got.Generation)
	assert.Equal(t, want.CreationTimestamp, got.CreationTimestamp)
	assert.Equal(t, want.DeletionTimestamp, got.DeletionTimestamp)
	assert.Equal(t, want.DeletionGracePeriodSeconds, got.DeletionGracePeriodSeconds)
	assert.Equal(t, want.Labels, got.Labels)
	assert.Equal(t, want.OwnerReferences, got.OwnerReferences)
	assert.Equal(t, want.Finalizers, got.Finalizers)
	assert.Nil(t, got.ManagedFields)
}

func Test_StripDownNetworkPolicyTransformFunc(t *testing.T) {
	tests := []struct {
		name string
		np   *networkingv1.NetworkPolicy
	}{
		{name: "full object with last-applied and managedFields", np: sampleNP("np-full")},
		{name: "no annotations, no deletion", np: func() *networkingv1.NetworkPolicy {
			np := sampleNP("np-bare")
			np.Annotations = nil
			np.DeletionTimestamp = nil
			np.ManagedFields = nil
			return np
		}()},
		{name: "empty spec (deny-all)", np: func() *networkingv1.NetworkPolicy {
			np := sampleNP("np-deny")
			np.Spec = networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}
			return np
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.np.DeepCopy()
			out, err := StripDownNetworkPolicyTransformFunc(tt.np)
			require.NoError(t, err)
			got := out.(*networkingv1.NetworkPolicy)
			assert.Equal(t, want.Spec, got.Spec, "spec must be kept verbatim")
			assert.Equal(t, want.TypeMeta, got.TypeMeta)
			assertKeptMeta(t, want.ObjectMeta, got.ObjectMeta)
			assert.Nil(t, got.Annotations, "NPC reads no NetworkPolicy annotation")

			again, err := StripDownNetworkPolicyTransformFunc(got.DeepCopy())
			require.NoError(t, err)
			assert.Equal(t, got, again, "transform must be idempotent")
		})
	}
}

func Test_StripDownPolicyEndpointTransformFunc(t *testing.T) {
	tests := []struct {
		name            string
		pe              *policyinfo.PolicyEndpoint
		wantAnnotations map[string]string
	}{
		{name: "empty PE keeps trigger-time annotation only", pe: samplePE("pe-empty", 0, 0),
			wantAnnotations: map[string]string{LastChangeTriggerTimeAnnotation: "2026-10-28T07:00:00.123456789Z"}},
		{name: "100 ingress / 100 pods", pe: samplePE("pe-100", 100, 100),
			wantAnnotations: map[string]string{LastChangeTriggerTimeAnnotation: "2026-10-28T07:00:00.123456789Z"}},
		{name: "full 1000-endpoint chunk", pe: samplePE("pe-1000", 1000, 1000),
			wantAnnotations: map[string]string{LastChangeTriggerTimeAnnotation: "2026-10-28T07:00:00.123456789Z"}},
		{name: "no trigger-time annotation drops the map", pe: func() *policyinfo.PolicyEndpoint {
			pe := samplePE("pe-noann", 1, 1)
			pe.Annotations = map[string]string{"other": "value"}
			return pe
		}(), wantAnnotations: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.pe.DeepCopy()
			out, err := StripDownPolicyEndpointTransformFunc(tt.pe)
			require.NoError(t, err)
			got := out.(*policyinfo.PolicyEndpoint)
			assert.Equal(t, want.Spec, got.Spec, "spec must be kept verbatim")
			assert.True(t, policyequality.EqualPolicyEndpointSpec(want.Spec, got.Spec),
				"EqualPolicyEndpointSpec must compare equal before and after")
			assert.True(t, policyequality.EqualPolicyEndpointSpec(got.Spec, want.Spec))
			assert.Equal(t, want.TypeMeta, got.TypeMeta)
			assertKeptMeta(t, want.ObjectMeta, got.ObjectMeta)
			assert.Equal(t, tt.wantAnnotations, got.Annotations)

			again, err := StripDownPolicyEndpointTransformFunc(got.DeepCopy())
			require.NoError(t, err)
			assert.Equal(t, got, again, "transform must be idempotent")
		})
	}
}

func Test_StripDownTransforms_passThroughOtherTypes(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{"a": "b"}}}
	tomb := cache.DeletedFinalStateUnknown{Key: "ns-a/np", Obj: sampleNP("np")}
	for _, fn := range []func(interface{}) (interface{}, error){
		StripDownNetworkPolicyTransformFunc, StripDownPolicyEndpointTransformFunc,
	} {
		out, err := fn(pod)
		require.NoError(t, err)
		assert.Same(t, pod, out)
		assert.Equal(t, map[string]string{"a": "b"}, out.(*corev1.Pod).Annotations)
		out, err = fn(tomb)
		require.NoError(t, err)
		assert.Equal(t, tomb, out)
	}
	cpe := &policyinfo.ClusterPolicyEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: map[string]string{"a": "b"}}}
	out, err := StripDownPolicyEndpointTransformFunc(cpe)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "b"}, out.(*policyinfo.ClusterPolicyEndpoint).Annotations,
		"ClusterPolicyEndpoint is written with Update and must not be trimmed")
}

// Test_finalizerPatch_onStrippedNetworkPolicy shows the finalizer patch built from a cached
// (stripped) object touches only finalizers and the optimistic-lock resourceVersion.
func Test_finalizerPatch_onStrippedNetworkPolicy(t *testing.T) {
	out, _ := StripDownNetworkPolicyTransformFunc(sampleNP("np"))
	np := out.(*networkingv1.NetworkPolicy)
	np.Finalizers = nil
	old := np.DeepCopy()
	controllerutil.AddFinalizer(np, "networking.k8s.aws/resources")
	data, err := client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{}).Data(np)
	require.NoError(t, err)
	assert.JSONEq(t, `{"metadata":{"finalizers":["networking.k8s.aws/resources"],"resourceVersion":"4242"}}`, string(data))
}

func objHeapBytes(t *testing.T, n int, decode func() interface{}) uint64 {
	t.Helper()
	keep := make([]interface{}, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range keep {
		keep[i] = decode()
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(keep)
	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return (after.HeapAlloc - before.HeapAlloc) / uint64(n)
}

// Test_transformFootprint measures retained heap per cached object before and after the transform.
func Test_transformFootprint(t *testing.T) {
	type sample struct {
		name string
		raw  []byte
		mk   func() interface{}
		tf   func(interface{}) (interface{}, error)
	}
	npRaw, _ := json.Marshal(sampleNP("np-footprint"))
	var samples []sample
	samples = append(samples, sample{"NetworkPolicy (kubectl apply)", npRaw,
		func() interface{} { return &networkingv1.NetworkPolicy{} }, StripDownNetworkPolicyTransformFunc})
	for _, sz := range []int{0, 100, 1000} {
		raw, _ := json.Marshal(samplePE("pe-footprint", sz, sz))
		samples = append(samples, sample{fmt.Sprintf("PolicyEndpoint %d/%d", sz, sz), raw,
			func() interface{} { return &policyinfo.PolicyEndpoint{} }, StripDownPolicyEndpointTransformFunc})
	}
	for _, s := range samples {
		n := 500
		if len(s.raw) > 50000 {
			n = 20
		}
		decode := func(transform bool) func() interface{} {
			return func() interface{} {
				obj := s.mk()
				if err := json.Unmarshal(s.raw, obj); err != nil {
					t.Fatal(err)
				}
				if transform {
					obj, _ = s.tf(obj)
				}
				return obj
			}
		}
		full := objHeapBytes(t, n, decode(false))
		trimmed := objHeapBytes(t, n, decode(true))
		t.Logf("FOOTPRINT %-30s json=%7dB heap/obj before=%7dB after=%7dB saved=%6dB (%.1f%%)",
			s.name, len(s.raw), full, trimmed, int64(full)-int64(trimmed), 100*float64(int64(full)-int64(trimmed))/float64(full))
		assert.Less(t, trimmed, full, s.name)
	}
}
