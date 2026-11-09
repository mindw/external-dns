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
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/cache"
)

// Taints that autoscalers add to a Node once they have decided to remove it.
const (
	// toBeDeletedByClusterAutoscalerTaint is added by the Cluster Autoscaler before it drains and deletes a Node.
	toBeDeletedByClusterAutoscalerTaint = "ToBeDeletedByClusterAutoscaler"
	// karpenterDisruptedTaint is added by Karpenter before it drains and deletes a Node.
	karpenterDisruptedTaint = "karpenter.sh/disrupted"
)

// unschedulableReason returns why --exclude-unschedulable skips the Node, or "" if it doesn't:
// the Node is cordoned, is being deleted, or has a taint an autoscaler adds before removing it.
func unschedulableReason(node *v1.Node) string {
	if node.Spec.Unschedulable {
		return "unschedulable"
	}
	if node.DeletionTimestamp != nil {
		return "being deleted"
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == toBeDeletedByClusterAutoscalerTaint || taint.Key == karpenterDisruptedTaint {
			return "tainted " + taint.Key
		}
	}
	return ""
}

// isExcludedByLabel reports whether the Node has the label key set to keep it out of NodePort targets.
// The key comes from --nodeport-exclude-label (default node.kubernetes.io/exclude-from-external-load-balancers,
// the label for load balancers that use Nodes as a second hop); an empty key never excludes.
// Values follow the cloud-provider service controller: any value other than one that parses as false
// excludes the Node.
func isExcludedByLabel(node *v1.Node, key string) bool {
	if key == "" {
		return false
	}
	value, ok := node.Labels[key]
	if !ok {
		return false
	}
	exclude, err := strconv.ParseBool(value)
	return err != nil || exclude
}

// nodeEventHandler calls handler when a Node is added or deleted, and when an update changes
// something the NodePort targets depend on. Other updates, such as status reports, are ignored.
func nodeEventHandler(handler func()) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ any) { handler() },
		UpdateFunc: func(oldObj, newObj any) {
			if nodeTargetsChanged(oldObj, newObj) {
				handler()
			}
		},
		DeleteFunc: func(_ any) { handler() },
	}
}

// nodeTargetsChanged reports whether an update changed the Node's addresses, labels, taints,
// unschedulable flag or deletion state. Objects that are not Nodes count as changed.
func nodeTargetsChanged(oldObj, newObj any) bool {
	oldNode, ok := oldObj.(*v1.Node)
	if !ok {
		return true
	}
	newNode, ok := newObj.(*v1.Node)
	if !ok {
		return true
	}
	return !equality.Semantic.DeepEqual(oldNode.Status.Addresses, newNode.Status.Addresses) ||
		!equality.Semantic.DeepEqual(oldNode.Labels, newNode.Labels) ||
		!equality.Semantic.DeepEqual(oldNode.Spec.Taints, newNode.Spec.Taints) ||
		oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable ||
		(oldNode.DeletionTimestamp == nil) != (newNode.DeletionTimestamp == nil)
}
