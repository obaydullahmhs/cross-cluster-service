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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Documentation that does not apply is worse than no documentation, so every
// shipped sample is applied against a real apiserver.
var _ = Describe("config/samples", func() {
	It("applies every sample manifest", func() {
		dir := filepath.Join("..", "..", "config", "samples")
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())

		applied := 0
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "kustomization.yaml" {
				continue
			}

			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			Expect(err).NotTo(HaveOccurred())

			for doc := range strings.SplitSeq(string(raw), "\n---\n") {
				if strings.TrimSpace(doc) == "" {
					continue
				}

				obj := &unstructured.Unstructured{}
				Expect(yaml.Unmarshal([]byte(doc), obj)).To(Succeed(), "sample %s", entry.Name())

				// Samples are namespaced by kustomize at deploy time; the
				// cluster-scoped kind must not carry one.
				if obj.GetKind() == "CrossService" {
					obj.SetNamespace(testNamespace)
				}

				Expect(k8sClient.Create(ctx, obj)).To(Succeed(),
					"sample %s/%s failed validation", entry.Name(), obj.GetName())
				applied++
			}
		}

		Expect(applied).To(BeNumerically(">=", 7), "expected samples to cover every source and Phase-1 access type")
	})
})
