/*
Copyright 2026.

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

package api

import (
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unstructuredOf re-reads an object from the apiserver as unstructured, so a
// spec can be asserted on the wire shape that was actually stored rather than
// on the Go struct -- which is the only way to prove a field is absent from the
// schema rather than merely unset.
func unstructuredOf(obj client.Object) map[string]any {
	gvks, _, err := k8sClient.Scheme().ObjectKinds(obj)
	Expect(err).NotTo(HaveOccurred())
	Expect(gvks).NotTo(BeEmpty())

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(gvks[0])
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), fetched)).To(Succeed())
	return fetched.Object
}

// digInto walks a nested map, failing the spec if any step is missing.
func digInto(obj map[string]any, path ...string) map[string]any {
	cur := obj
	for _, step := range path {
		next, ok := cur[step]
		ExpectWithOffset(1, ok).To(BeTrue(), "expected key %q at %v", step, path)
		cur, ok = next.(map[string]any)
		ExpectWithOffset(1, ok).To(BeTrue(), "expected %q to be an object at %v", step, path)
	}
	return cur
}
