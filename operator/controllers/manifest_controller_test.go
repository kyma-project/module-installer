package controllers_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"errors"
	"github.com/kyma-project/module-manager/operator/api/v1alpha1"
	"github.com/kyma-project/module-manager/operator/pkg/types"
	"github.com/kyma-project/module-manager/operator/pkg/util"
)

const (
	Timeout  = time.Second * 30
	Interval = time.Millisecond * 250
)

func createManifestWithHelmRepo() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with a Helm Chart")
		helmChartSpec := types.HelmChartSpec{
			ChartName: "nginx-ingress",
			URL:       "https://helm.nginx.com/stable",
			Type:      "helm-chart",
		}
		specBytes, err := json.Marshal(helmChartSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "nginx-stable",
				},
			},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		deleteManifestResource(manifestObj, nil)

		return true
	}
}

func createManifestWithOCI() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with an OCI specification")
		imageSpec := GetImageSpecFromMockOCIRegistry()

		specBytes, err := json.Marshal(imageSpec)
		Expect(err).ToNot(HaveOccurred())

		// initial HelmClient cache entry
		kymaNsName := client.ObjectKey{Name: secretName, Namespace: v1.NamespaceDefault}
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).Should(BeNil())

		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "oci-image",
				},
			},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		// intermediate HelmClient cache entry
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).ShouldNot(BeNil())
		deleteHelmChartResources(imageSpec)
		deleteManifestResource(manifestObj, nil)

		// create another manifest with same image specification
		manifestObj2 := createManifestObj("manifest-sample-2", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "oci-image",
				},
			},
		})

		Expect(k8sClient.Create(ctx, manifestObj2)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj2)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		verifyHelmResourcesDeletion(imageSpec)

		deleteManifestResource(manifestObj2, nil)

		// final HelmClient cache entry
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).Should(BeNil())

		Expect(os.RemoveAll(util.GetFsChartPath(imageSpec))).Should(Succeed())
		return true
	}
}

func createTwoRemoteManifestsWithNoInstalls() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with an OCI specification")
		imageSpec := GetImageSpecFromMockOCIRegistry()
		kymaNsName := client.ObjectKey{Name: secretName, Namespace: v1.NamespaceDefault}

		// verify cluster cache empty
		Expect(reconciler.CacheManager.ClusterInfos.Get(kymaNsName).IsEmpty()).To(BeTrue())

		// creating cluster cache entry
		reconciler.CacheManager.ClusterInfos.Set(kymaNsName, types.ClusterInfo{Config: cfg})

		kymaSecret := createKymaSecret()
		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Remote:   true,
			Installs: []v1alpha1.InstallInfo{},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		// check client cache entries after 1st resource creation
		Expect(reconciler.CacheManager.ClusterInfos.Get(kymaNsName).Config).To(BeEquivalentTo(cfg))
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).Should(BeNil()) // no Installs exist

		// create another manifest with same image specification
		manifestObj2 := createManifestObj("manifest-sample-2", v1alpha1.ManifestSpec{
			Remote:   true,
			Installs: []v1alpha1.InstallInfo{},
		})

		Expect(k8sClient.Create(ctx, manifestObj2)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj2)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		verifyHelmResourcesDeletion(imageSpec)
		deleteManifestResource(manifestObj, nil)

		// check client cache entries after 2nd resource creation
		Expect(reconciler.CacheManager.ClusterInfos.Get(kymaNsName).Config).To(BeEquivalentTo(cfg))
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).Should(BeNil()) // no Installs exist

		deleteManifestResource(manifestObj2, kymaSecret)

		// verify client cache deleted
		Expect(reconciler.CacheManager.ClusterInfos.Get(kymaNsName).IsEmpty()).To(BeTrue())
		Expect(reconciler.CacheManager.RenderSources.Get(kymaNsName)).Should(BeNil()) // no Installs exist
		return true
	}
}

func createManifestWithInvalidOCI() func() bool {
	return func() bool {
		By("having transitioned the CR State to Error with invalid OCI Specification")
		imageSpec := GetImageSpecFromMockOCIRegistry()
		imageSpec.Repo = "invalid.com"

		specBytes, err := json.Marshal(imageSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "oci-image",
				},
			},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateError))

		deleteManifestResource(manifestObj, nil)

		Expect(os.RemoveAll(util.GetFsChartPath(imageSpec))).Should(Succeed())
		return true
	}
}

func createManifestWithRemoteKustomize() func() bool {
	return func() bool {
		kustomizeSpec := types.KustomizeSpec{
			URL:  "https://github.com/kyma-project/module-manager//operator/config/default?ref=main",
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())

		manifestObj := createManifest(specBytes, "manifest-sample")
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), Timeout, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		deleteManifestResource(manifestObj, nil)

		return true
	}
}

func createManifest(specBytes []byte, manifestName string) *v1alpha1.Manifest {
	manifestObj := createManifestObj(manifestName, v1alpha1.ManifestSpec{
		Installs: []v1alpha1.InstallInfo{
			{
				Source: runtime.RawExtension{
					Raw: specBytes,
				},
				Name: manifestName,
			},
		},
	})
	Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
	return manifestObj
}

func createManifestWithLocalKustomize() func() bool {
	return func() bool {
		kustomizeSpec := types.KustomizeSpec{
			Path: "./test_samples/kustomize",
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())

		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "kustomize-test",
				},
			},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateReady))

		deleteManifestResource(manifestObj, nil)

		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func createManifestWithInvalidKustomize() func() bool {
	return func() bool {
		kustomizeSpec := types.KustomizeSpec{
			Path: "./invalidPath",
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())

		manifestObj := createManifestObj("manifest-sample", v1alpha1.ManifestSpec{
			Installs: []v1alpha1.InstallInfo{
				{
					Source: runtime.RawExtension{
						Raw: specBytes,
					},
					Name: "kustomize-test",
				},
			},
		})
		Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
		//Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), 5*time.Minute, 250*time.Millisecond).
		//	Should(BeEquivalentTo(v1alpha1.ManifestStateError))

		deleteManifestResource(manifestObj, nil)

		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func getManifestState(manifestName string) v1alpha1.ManifestState {
	manifest := &v1alpha1.Manifest{}

	err := k8sClient.Get(ctx, client.ObjectKey{
		Namespace: v1.NamespaceDefault,
		Name:      manifestName,
	}, manifest)
	if err != nil {
		return "invalid"
	}
	return manifest.Status.State
}

func getManifest(key client.ObjectKey) func() bool {
	return func() bool {
		manifest := v1alpha1.Manifest{}
		err := k8sClient.Get(ctx, key, &manifest)
		return apiErrors.IsNotFound(err)
	}
}

func setHelmEnv() error {
	os.Setenv(helmCacheHomeEnv, helmCacheHome)
	os.Setenv(helmCacheRepoEnv, helmCacheRepo)
	os.Setenv(helmRepoEnv, helmRepoFile)
	return nil
}

func unsetHelmEnv() error {
	os.Unsetenv(helmCacheHomeEnv)
	os.Unsetenv(helmCacheRepoEnv)
	os.Unsetenv(helmRepoEnv)
	return nil
}

//var _ = Describe("given manifest with a helm repo", func() {
//	BeforeEach(func() {
//		Expect(setHelmEnv()).Should(Succeed())
//	})
//
//	DescribeTable("given watcherCR reconcile loop",
//		func(testCaseFn func() bool) {
//			Expect(testCaseFn()).To(BeTrue())
//		},
//		[]TableEntry{
//			//Entry("when manifestCR contains a valid remote Kustomize specification", createManifestWithRemoteKustomize()),
//			//Entry("when manifestCR contains a valid local Kustomize specification", createManifestWithLocalKustomize()),
//			//Entry("when manifestCR contains invalid Kustomize specification", createManifestWithInvalidKustomize()),
//			//Entry("when manifestCR contains a valid helm repo", createManifestWithHelmRepo()),
//			//Entry("when two manifestCRs contain valid OCI Image specification", createManifestWithOCI()),
//			//Entry("when manifestCR contains invalid OCI Image specification", createManifestWithInvalidOCI()),
//			//Entry("when two remote manifestCRs contain no install specification", createTwoRemoteManifestsWithNoInstalls()),
//			// TODO write tests for pre-rendered Manifests
//		})
//
//	AfterEach(func() {
//		//Expect(unsetHelmEnv()).Should(Succeed())
//	})
//})

var _ = Describe("given manifest with kustomize", func() {
	remoteKustomizeSpec := types.KustomizeSpec{
		URL:  "https://github.com/kyma-project/module-manager//operator/config/default?ref=main",
		Type: "kustomize",
	}
	localKustomizeSpec := types.KustomizeSpec{
		Path: "./test_samples/kustomize",
		Type: "kustomize",
	}
	invalidKustomizeSpec := types.KustomizeSpec{
		Path: "./invalidPath",
		Type: "kustomize",
	}

	DescribeTable("Test ModuleStatus",
		func(givenCondition func(manifest *v1alpha1.Manifest) error, expectedBehavior func(manifestName string) error) {
			var manifest = NewTestManifest("manifest")
			Eventually(givenCondition, Timeout, Interval).WithArguments(manifest).Should(Succeed())
			Eventually(expectedBehavior, Timeout, Interval).WithArguments(manifest.GetName()).Should(Succeed())
		},
		Entry("When manifestCR contains a valid remote Kustomize specification, expect state in ready",
			addKustomizeSpec(remoteKustomizeSpec), expectManifestStateIn(v1alpha1.ManifestStateReady)),
		Entry("When manifestCR contains a valid local Kustomize specification, expect state in ready",
			addKustomizeSpec(localKustomizeSpec), expectManifestStateIn(v1alpha1.ManifestStateReady)),
		Entry("When manifestCR contains an invalid local Kustomize specification, expect state in error",
			addKustomizeSpec(invalidKustomizeSpec), expectManifestStateIn(v1alpha1.ManifestStateError)),
	)

})
var _ = Describe("given manifest with helm repo", func() {
	setHelmEnv()
	validHelmChartSpec := types.HelmChartSpec{
		ChartName: "nginx-ingress",
		URL:       "https://helm.nginx.com/stable",
		Type:      "helm-chart",
	}
	DescribeTable("Test ModuleStatus",
		func(givenCondition func(manifest *v1alpha1.Manifest) error, expectedBehavior func(manifestName string) error) {
			var manifest = NewTestManifest("manifest")
			Eventually(givenCondition, Timeout, Interval).WithArguments(manifest).Should(Succeed())
			Eventually(expectedBehavior, Timeout, Interval).WithArguments(manifest.GetName()).Should(Succeed())
		},
		Entry("When manifestCR contains a valid helm repo, expect state in ready",
			addHelmChartSpec(validHelmChartSpec), expectManifestStateIn(v1alpha1.ManifestStateReady)),
	)
})

func expectManifestStateIn(state v1alpha1.ManifestState) func(manifestName string) error {
	return func(manifestName string) error {
		manifestState := getManifestState(manifestName)
		if state != manifestState {
			return errors.New("ManifestState not match")
		}
		return nil
	}
}

func addKustomizeSpec(spec types.KustomizeSpec) func(manifest *v1alpha1.Manifest) error {
	return func(manifest *v1alpha1.Manifest) error {
		specBytes, err := json.Marshal(spec)
		Expect(err).ToNot(HaveOccurred())
		manifest.Spec.Installs = []v1alpha1.InstallInfo{
			{
				Source: runtime.RawExtension{
					Raw: specBytes,
				},
				Name: "kustomize-test",
			},
		}
		return k8sClient.Create(ctx, manifest)
	}
}

func addHelmChartSpec(spec types.HelmChartSpec) func(manifest *v1alpha1.Manifest) error {
	return func(manifest *v1alpha1.Manifest) error {
		specBytes, err := json.Marshal(spec)
		Expect(err).ToNot(HaveOccurred())
		manifest.Spec.Installs = []v1alpha1.InstallInfo{
			{
				Source: runtime.RawExtension{
					Raw: specBytes,
				},
				Name: "nginx-stable",
			},
		}
		return k8sClient.Create(ctx, manifest)
	}
}
