/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package source

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/internal/testutils"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/source/annotations"
	"sigs.k8s.io/external-dns/source/informers"
	templatetest "sigs.k8s.io/external-dns/source/template/testutil"
)

// customExcludeLabel is an external-dns-specific label used to exclude Nodes instead of the standard one.
const customExcludeLabel = "external-dns.kubernetes.io/exclude"

// TestNewSourceConfigNodePortExcludeLabel checks that --nodeport-exclude-label reaches the source config.
// Losing this mapping in a rebase would turn the exclusion off silently, since "" means disabled.
func TestNewSourceConfigNodePortExcludeLabel(t *testing.T) {
	got, err := NewSourceConfig(&externaldns.Config{NodePortExcludeLabel: "karpenter.sh/nodepool"})
	require.NoError(t, err)
	assert.Equal(t, "karpenter.sh/nodepool", got.NodePortExcludeLabel)
}

// TestServiceSourceNodePortExcludeLabel checks that --nodeport-exclude-label picks the label that keeps
// Nodes out of NodePort targets, and that an empty value turns the label check off.
func TestServiceSourceNodePortExcludeLabel(t *testing.T) {
	t.Parallel()

	nodes := []*v1.Node{{
		Name:   "node1",
		Labels: map[string]string{v1.LabelNodeExcludeBalancers: "true"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "54.10.11.1"}}},
	}, {
		Name:   "node2",
		Labels: map[string]string{customExcludeLabel: "true"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "54.10.11.2"}}},
	}, {
		Name:   "node3",
		Labels: map[string]string{"karpenter.sh/nodepool": "default"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "54.10.11.3"}}},
	}}

	for _, tc := range []struct {
		title   string
		key     string
		targets endpoint.Targets
	}{
		{title: "default key skips Nodes with the standard label", key: v1.LabelNodeExcludeBalancers, targets: endpoint.Targets{"54.10.11.2", "54.10.11.3"}},
		{title: "empty key keeps every Node", key: "", targets: endpoint.Targets{"54.10.11.1", "54.10.11.2", "54.10.11.3"}},
		{title: "custom key skips Nodes with that label only", key: customExcludeLabel, targets: endpoint.Targets{"54.10.11.1", "54.10.11.3"}},
		{title: "karpenter.sh/nodepool skips Karpenter Nodes", key: "karpenter.sh/nodepool", targets: endpoint.Targets{"54.10.11.1", "54.10.11.2"}},
	} {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()

			kubernetes := fake.NewClientset()
			for _, node := range nodes {
				_, err := kubernetes.CoreV1().Nodes().Create(t.Context(), node, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			service := &v1.Service{
				Namespace:   "testing",
				Name:        "foo",
				Annotations: map[string]string{annotations.HostnameKey: "foo.example.org."},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeNodePort,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
					Ports:                 []v1.ServicePort{{NodePort: 30192}},
				},
			}
			_, err := kubernetes.CoreV1().Services(service.Namespace).Create(t.Context(), service, metav1.CreateOptions{})
			require.NoError(t, err)

			client, err := NewServiceSource(t.Context(), kubernetes, &Config{
				TemplateEngine:       templatetest.MustEngine(t, "", "", "", false),
				Namespaces:           []string{""},
				ExcludeUnschedulable: true,
				NodePortExcludeLabel: tc.key,
				LabelFilter:          labels.Everything(),
			})
			require.NoError(t, err)

			endpoints, err := client.Endpoints(t.Context())
			require.NoError(t, err)
			testutils.ValidateEndpoints(t, endpoints, []*endpoint.Endpoint{
				{DNSName: "_foo._tcp.foo.example.org", Targets: endpoint.Targets{"0 50 30192 foo.example.org."}, RecordType: endpoint.RecordTypeSRV},
				{DNSName: "foo.example.org", Targets: tc.targets, RecordType: endpoint.RecordTypeA},
			})
		})
	}
}

// TestNodesExternalTrafficPolicyTypeLocalExcludeLabel checks that with externalTrafficPolicy=Local a Node
// with the configured label is dropped before the readiness tiers are compared.
func TestNodesExternalTrafficPolicyTypeLocalExcludeLabel(t *testing.T) {
	client := fake.NewClientset()
	factory := kubeinformers.NewSharedInformerFactory(client, 0)
	nodeInformer := factory.Core().V1().Nodes()
	podInformer := factory.Core().V1().Pods()

	excluded := &v1.Node{
		Name:   "node1",
		Labels: map[string]string{customExcludeLabel: "true"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "54.10.11.1"}}},
	}
	other := &v1.Node{
		Name:   "node2",
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "54.10.11.2"}}},
	}
	require.NoError(t, nodeInformer.Informer().GetStore().Add(excluded))
	require.NoError(t, nodeInformer.Informer().GetStore().Add(other))

	pod := func(name, nodeName string, ready v1.ConditionStatus) *v1.Pod {
		return &v1.Pod{
			Name: name, Namespace: "testing",
			Spec: v1.PodSpec{NodeName: nodeName},
			Status: v1.PodStatus{
				Phase:      v1.PodRunning,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: ready}},
			},
		}
	}
	// The only ready pod runs on the excluded Node, so the not-ready pod's Node is used instead.
	require.NoError(t, podInformer.Informer().GetStore().Add(pod("pod-0", "node1", v1.ConditionTrue)))
	require.NoError(t, podInformer.Informer().GetStore().Add(pod("pod-1", "node2", v1.ConditionFalse)))

	sc := &serviceSource{
		podInformers:         informers.Single[coreinformers.PodInformer](podInformer),
		nodeInformer:         nodeInformer,
		nodePortExcludeLabel: customExcludeLabel,
	}
	svc := &v1.Service{
		Name: "foo", Namespace: "testing",
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeNodePort, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal},
	}

	got := sc.nodesExternalTrafficPolicyTypeLocal(svc)
	require.Len(t, got, 1)
	assert.Equal(t, "node2", got[0].Name)
}
