/*
Copyright 2025.

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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	medik8sv1alpha1 "github.com/medik8s/sbd-operator/api/v1alpha1"
)

var _ = Describe("SBDConfig Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-sbdconfig"

		ctx := context.Background()

		// Note: SBDConfig is cluster-scoped, so no namespace
		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}

		var controllerReconciler *SBDConfigReconciler

		BeforeEach(func() {
			By("initializing controller reconciler")
			controllerReconciler = &SBDConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the custom resource for the Kind SBDConfig")
			sbdconfig := &medik8sv1alpha1.SBDConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, sbdconfig)
			if err != nil && errors.IsNotFound(err) {
				resource := &medik8sv1alpha1.SBDConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name: resourceName,
					},
					Spec: medik8sv1alpha1.SBDConfigSpec{
						// Add basic spec if needed
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("cleaning up the specific resource instance SBDConfig")
			resource := &medik8sv1alpha1.SBDConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should successfully reconcile an existing resource", func() {
			By("reconciling the created resource")
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			By("verifying the resource still exists")
			sbdconfig := &medik8sv1alpha1.SBDConfig{}
			err = k8sClient.Get(ctx, typeNamespacedName, sbdconfig)
			Expect(err).NotTo(HaveOccurred())
			Expect(sbdconfig.Name).To(Equal(resourceName))
		})

		It("should handle reconciling a non-existent resource without error", func() {
			nonExistentName := types.NamespacedName{
				Name: "non-existent-sbdconfig",
			}

			By("reconciling a non-existent resource")
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nonExistentName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("should successfully reconcile after resource deletion", func() {
			By("deleting the resource first")
			resource := &medik8sv1alpha1.SBDConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling the deleted resource")
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("When testing controller initialization", func() {
		It("should create a controller reconciler successfully", func() {
			reconciler := &SBDConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			Expect(reconciler.Client).NotTo(BeNil())
			Expect(reconciler.Scheme).NotTo(BeNil())
		})
	})
})
