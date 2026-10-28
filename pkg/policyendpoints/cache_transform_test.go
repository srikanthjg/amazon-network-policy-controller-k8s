package policyendpoints

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyinfo "github.com/aws/amazon-network-policy-controller-k8s/api/v1alpha1"
	"github.com/aws/amazon-network-policy-controller-k8s/pkg/k8s"
)

func chunkedPEs(policy *networking.NetworkPolicy, total, chunk int) []policyinfo.PolicyEndpoint {
	var pes []policyinfo.PolicyEndpoint
	for start, idx := 0, 0; start < total; start, idx = start+chunk, idx+1 {
		pe := policyinfo.PolicyEndpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", policy.Name, idx), Namespace: policy.Namespace,
				ResourceVersion: "10", Labels: map[string]string{"k": "v"},
				Annotations: map[string]string{
					LastChangeTriggerTimeAnnotation: "2026-10-28T07:00:00Z",
					"other":                         "x",
				},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "controller", Operation: metav1.ManagedFieldsOperationUpdate}},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy",
					Name: policy.Name, UID: policy.UID}},
			},
			Spec: policyinfo.PolicyEndpointSpec{
				PodSelector:  &policy.Spec.PodSelector,
				PolicyRef:    policyinfo.PolicyReference{Namespace: policy.Namespace, Name: policy.Name},
				PodIsolation: policy.Spec.PolicyTypes,
			},
		}
		for i := start; i < start+chunk && i < total; i++ {
			pe.Spec.Ingress = append(pe.Spec.Ingress, policyinfo.EndpointInfo{
				CIDR: policyinfo.NetworkAddress(fmt.Sprintf("10.%d.%d.1/32", i/256, i%256))})
		}
		pes = append(pes, pe)
	}
	return pes
}

func stripAll(t *testing.T, in []policyinfo.PolicyEndpoint) []policyinfo.PolicyEndpoint {
	out := make([]policyinfo.PolicyEndpoint, 0, len(in))
	for i := range in {
		o, err := k8s.StripDownPolicyEndpointTransformFunc(in[i].DeepCopy())
		require.NoError(t, err)
		out = append(out, *o.(*policyinfo.PolicyEndpoint))
	}
	return out
}

type outcome struct {
	creates, updates, deletes int
	updateNames, deleteNames  []string
	ingress                   []string
}

func summarize(c, u, d []policyinfo.PolicyEndpoint) outcome {
	o := outcome{creates: len(c), updates: len(u), deletes: len(d)}
	for _, pe := range u {
		o.updateNames = append(o.updateNames, pe.Name)
	}
	for _, pe := range d {
		o.deleteNames = append(o.deleteNames, pe.Name)
	}
	for _, list := range [][]policyinfo.PolicyEndpoint{c, u} {
		for _, pe := range list {
			for _, ep := range pe.Spec.Ingress {
				o.ingress = append(o.ingress, string(ep.CIDR))
			}
		}
	}
	sort.Strings(o.updateNames)
	sort.Strings(o.deleteNames)
	sort.Strings(o.ingress)
	return o
}

// Test_computePolicyEndpoints_sameOnStrippedCache runs the bin-packing on raw and on cache-trimmed
// PolicyEndpoints (more than one 1000-endpoint chunk) and requires the same create/update/delete outcome.
func Test_computePolicyEndpoints_sameOnStrippedCache(t *testing.T) {
	policy := &networking.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "np", Namespace: "ns", UID: "uid"},
		Spec: networking.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress},
		},
	}
	tests := []struct {
		name         string
		existing     int
		desired      int
		desiredShift int
	}{
		{name: "steady state 2500 endpoints across 3 chunks", existing: 2500, desired: 2500},
		{name: "grow past a chunk boundary", existing: 2500, desired: 3200},
		{name: "shrink drops a chunk", existing: 2500, desired: 900},
		{name: "rotate half the endpoints", existing: 2500, desired: 2500, desiredShift: 1250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := chunkedPEs(policy, tt.existing, 1000)
			var desired []policyinfo.EndpointInfo
			for i := tt.desiredShift; i < tt.desiredShift+tt.desired; i++ {
				desired = append(desired, policyinfo.EndpointInfo{
					CIDR: policyinfo.NetworkAddress(fmt.Sprintf("10.%d.%d.1/32", i/256, i%256))})
			}
			m := &policyEndpointsManager{endpointChunkSize: 1000}
			c1, u1, d1, err := m.computePolicyEndpoints(policy, raw, desired, nil, nil)
			require.NoError(t, err)
			c2, u2, d2, err := m.computePolicyEndpoints(policy, stripAll(t, raw), desired, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, summarize(c1, u1, d1), summarize(c2, u2, d2))
		})
	}
}

// Test_updatePatch_fromStrippedCache shows the PE update patch built from a cached copy only carries
// the spec change and the trigger-time annotation: labels, owner refs and other annotations stay untouched.
func Test_updatePatch_fromStrippedCache(t *testing.T) {
	policy := &networking.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "np", Namespace: "ns", UID: "uid"}}
	cached := stripAll(t, chunkedPEs(policy, 2, 1000))[0]
	oldRes := cached.DeepCopy()
	updated := cached.DeepCopy()
	updated.Spec.Ingress = append(updated.Spec.Ingress, policyinfo.EndpointInfo{CIDR: "10.9.9.9/32"})
	updated.Annotations[LastChangeTriggerTimeAnnotation] = "2026-10-28T08:00:00Z"

	data, err := client.MergeFrom(oldRes).Data(updated)
	require.NoError(t, err)
	var patch map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &patch))
	assert.ElementsMatch(t, []string{"metadata", "spec"}, keys(patch))
	assert.Equal(t, map[string]interface{}{"annotations": map[string]interface{}{
		LastChangeTriggerTimeAnnotation: "2026-10-28T08:00:00Z"}}, patch["metadata"])
	assert.ElementsMatch(t, []string{"ingress"}, keys(patch["spec"].(map[string]interface{})))
}

func keys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
