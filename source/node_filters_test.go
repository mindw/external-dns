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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestUnschedulableReason(t *testing.T) {
	now := metav1.Now()

	for _, tc := range []struct {
		name string
		node *v1.Node
		want string
	}{
		{
			name: "schedulable node",
			node: &v1.Node{Name: "node1"},
			want: "",
		},
		{
			name: "cordoned node",
			node: &v1.Node{Name: "node1", Spec: v1.NodeSpec{Unschedulable: true}},
			want: "unschedulable",
		},
		{
			name: "node being deleted",
			node: &v1.Node{Name: "node1", DeletionTimestamp: &now},
			want: "being deleted",
		},
		{
			name: "node tainted by the Cluster Autoscaler",
			node: &v1.Node{Name: "node1", Spec: v1.NodeSpec{Taints: []v1.Taint{
				{Key: "ToBeDeletedByClusterAutoscaler", Value: "1700000000", Effect: v1.TaintEffectNoSchedule},
			}}},
			want: "tainted ToBeDeletedByClusterAutoscaler",
		},
		{
			name: "node tainted by Karpenter",
			node: &v1.Node{Name: "node1", Spec: v1.NodeSpec{Taints: []v1.Taint{
				{Key: "karpenter.sh/disrupted", Effect: v1.TaintEffectNoSchedule},
			}}},
			want: "tainted karpenter.sh/disrupted",
		},
		{
			name: "other taints are ignored",
			node: &v1.Node{Name: "node1", Spec: v1.NodeSpec{Taints: []v1.Taint{
				{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoExecute},
				{Key: "dedicated", Value: "edge", Effect: v1.TaintEffectNoSchedule},
			}}},
			want: "",
		},
		{
			name: "cordon is reported before a removal taint",
			node: &v1.Node{Name: "node1", Spec: v1.NodeSpec{
				Unschedulable: true,
				Taints:        []v1.Taint{{Key: "karpenter.sh/disrupted", Effect: v1.TaintEffectNoSchedule}},
			}},
			want: "unschedulable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unschedulableReason(tc.node))
		})
	}
}

func TestIsExcludedByLabel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "no labels", labels: nil, want: false},
		{name: "other labels", labels: map[string]string{"karpenter.sh/nodepool": "default"}, want: false},
		{name: "true", labels: map[string]string{v1.LabelNodeExcludeBalancers: "true"}, want: true},
		{name: "True", labels: map[string]string{v1.LabelNodeExcludeBalancers: "True"}, want: true},
		{name: "1", labels: map[string]string{v1.LabelNodeExcludeBalancers: "1"}, want: true},
		{name: "false", labels: map[string]string{v1.LabelNodeExcludeBalancers: "false"}, want: false},
		{name: "0", labels: map[string]string{v1.LabelNodeExcludeBalancers: "0"}, want: false},
		// Same as the cloud-provider service controller: a value that isn't a bool excludes the Node.
		{name: "empty value", labels: map[string]string{v1.LabelNodeExcludeBalancers: ""}, want: true},
		{name: "unparsable value", labels: map[string]string{v1.LabelNodeExcludeBalancers: "yes"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isExcludedByLabel(&v1.Node{Name: "node1", Labels: tc.labels}, v1.LabelNodeExcludeBalancers))
		})
	}
}

// TestIsExcludedByLabelKey covers the fork-only --nodeport-exclude-label: the key decides which label
// is read, an empty key turns the check off, and values follow the same rule as the standard label.
func TestIsExcludedByLabelKey(t *testing.T) {
	const customKey = "external-dns.kubernetes.io/exclude"
	for _, tc := range []struct {
		name   string
		key    string
		labels map[string]string
		want   bool
	}{
		{name: "empty key ignores the standard label", key: "", labels: map[string]string{v1.LabelNodeExcludeBalancers: "true"}, want: false},
		{name: "custom key true", key: customKey, labels: map[string]string{customKey: "true"}, want: true},
		{name: "custom key false", key: customKey, labels: map[string]string{customKey: "false"}, want: false},
		{name: "custom key ignores the standard label", key: customKey, labels: map[string]string{v1.LabelNodeExcludeBalancers: "true"}, want: false},
		// Pool names aren't booleans, so every Node of any Karpenter NodePool is excluded.
		{name: "karpenter nodepool name", key: "karpenter.sh/nodepool", labels: map[string]string{"karpenter.sh/nodepool": "default"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isExcludedByLabel(&v1.Node{Name: "node1", Labels: tc.labels}, tc.key))
		})
	}
}

func TestNodeEventHandler(t *testing.T) {
	now := metav1.Now()

	newNode := func() *v1.Node {
		return &v1.Node{
			Name:            "node1",
			ResourceVersion: "1",
			Labels:          map[string]string{"kubernetes.io/hostname": "node1"},
			Annotations:     map[string]string{"node.alpha.kubernetes.io/ttl": "0"},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}},
			},
		}
	}

	for _, tc := range []struct {
		name   string
		update func(n *v1.Node)
		want   int
	}{
		{
			name: "resourceVersion, annotations and status other than addresses are ignored",
			update: func(n *v1.Node) {
				n.ResourceVersion = "2"
				n.Annotations["node.alpha.kubernetes.io/ttl"] = "15"
				n.Status.Images = []v1.ContainerImage{{Names: []string{"busybox"}}}
				n.Status.Phase = v1.NodeRunning
			},
			want: 0,
		},
		{
			name:   "address change",
			update: func(n *v1.Node) { n.Status.Addresses[0].Address = "10.0.0.2" },
			want:   1,
		},
		{
			name:   "label change",
			update: func(n *v1.Node) { n.Labels[v1.LabelNodeExcludeBalancers] = "true" },
			want:   1,
		},
		{
			name: "taint added",
			update: func(n *v1.Node) {
				n.Spec.Taints = []v1.Taint{{Key: "karpenter.sh/disrupted", Effect: v1.TaintEffectNoSchedule}}
			},
			want: 1,
		},
		{
			name:   "cordoned",
			update: func(n *v1.Node) { n.Spec.Unschedulable = true },
			want:   1,
		},
		{
			name:   "deletion started",
			update: func(n *v1.Node) { n.DeletionTimestamp = &now },
			want:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := nodeEventHandler(func() { calls++ })

			oldNode, updated := newNode(), newNode()
			tc.update(updated)
			handler.OnUpdate(oldNode, updated)

			assert.Equal(t, tc.want, calls)
		})
	}

	t.Run("add and delete always trigger", func(t *testing.T) {
		calls := 0
		handler := nodeEventHandler(func() { calls++ })

		handler.OnAdd(newNode(), false)
		handler.OnDelete(newNode())
		handler.OnDelete(cache.DeletedFinalStateUnknown{Key: "node1", Obj: newNode()})

		assert.Equal(t, 3, calls)
	})

	t.Run("update with objects that are not Nodes triggers", func(t *testing.T) {
		calls := 0
		handler := nodeEventHandler(func() { calls++ })

		handler.OnUpdate(nil, newNode())

		assert.Equal(t, 1, calls)
	})
}
