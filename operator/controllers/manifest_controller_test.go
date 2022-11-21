package controllers_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/module-manager/operator/api/v1alpha1"
	"github.com/kyma-project/module-manager/operator/pkg/types"
	"github.com/kyma-project/module-manager/operator/pkg/util"
)

func createManifestAndCheckState(desiredState v1alpha1.ManifestState, specBytes []byte, installName string,
	remote bool,
) *v1alpha1.Manifest {
	installs := make([]v1alpha1.InstallInfo, 0)
	if specBytes != nil {
		installs = append(installs, v1alpha1.InstallInfo{
			Source: runtime.RawExtension{
				Raw: specBytes,
			},
			Name: installName,
		})
	}
	manifestObj := createManifestObj(string(uuid.NewUUID()), v1alpha1.ManifestSpec{
		Remote:   remote,
		Installs: installs,
	})
	Expect(k8sClient.Create(ctx, manifestObj)).Should(Succeed())
	Eventually(getManifestState(client.ObjectKeyFromObject(manifestObj)), standardTimeout, standardInterval).
		Should(BeEquivalentTo(desiredState))
	return manifestObj
}

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
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"nginx-stable", false)
		deleteManifestResource(manifestObj, nil)
		return true
	}
}

func createManifestWithOCI() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with an OCI specification")
		// spec
		imageSpec := GetImageSpecFromMockOCIRegistry()
		specBytes, err := json.Marshal(imageSpec)
		Expect(err).ToNot(HaveOccurred())
		// initial HelmClient cache entry
		kymaNsName := client.ObjectKey{Name: secretName, Namespace: v1.NamespaceDefault}
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).Should(BeNil())
		// resource
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"oci-image", false)
		// intermediate HelmClient cache entry
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).ShouldNot(BeNil())
		deleteHelmChartResources(imageSpec)
		deleteManifestResource(manifestObj, nil)
		// create another manifest with same image specification
		manifestObj2 := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"oci-image", false)
		verifyHelmResourcesDeletion(imageSpec)
		deleteManifestResource(manifestObj2, nil)
		// final HelmClient cache entry
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).Should(BeNil())
		Expect(os.RemoveAll(util.GetFsChartPath(imageSpec))).Should(Succeed())
		return true
	}
}

func createTwoRemoteManifestsWithNoInstalls() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with an OCI spec and no installs")
		kymaNsName := client.ObjectKey{Name: secretName, Namespace: v1.NamespaceDefault}
		// verify cluster cache empty
		Expect(reconciler.CacheManager.GetClusterInfoCache().Get(kymaNsName).IsEmpty()).To(BeTrue())
		// creating cluster cache entry
		reconciler.CacheManager.GetClusterInfoCache().Set(kymaNsName, types.ClusterInfo{Config: cfg})
		kymaSecret := createKymaSecret()
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, nil,
			"", true)
		// check client cache entries after 1st resource creation
		Expect(reconciler.CacheManager.GetClusterInfoCache().Get(kymaNsName).Config).To(BeEquivalentTo(cfg))
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).Should(BeNil()) // no Installs exist
		// create another manifest with same image specification
		manifestObj2 := createManifestAndCheckState(v1alpha1.ManifestStateReady, nil,
			"", true)
		deleteManifestResource(manifestObj, nil)
		// check client cache entries after 2nd resource creation
		Expect(reconciler.CacheManager.GetClusterInfoCache().Get(kymaNsName).Config).To(BeEquivalentTo(cfg))
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).Should(BeNil()) // no Installs exist
		deleteManifestResource(manifestObj2, kymaSecret)
		// verify client cache deleted
		Expect(reconciler.CacheManager.GetClusterInfoCache().Get(kymaNsName).IsEmpty()).To(BeTrue())
		Expect(reconciler.CacheManager.GetRendererCache().GetProcessor(kymaNsName)).Should(BeNil()) // no Installs exist
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
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateError, specBytes,
			"oci-image", false)

		deleteManifestResource(manifestObj, nil)

		Expect(os.RemoveAll(util.GetFsChartPath(imageSpec))).Should(Succeed())
		return true
	}
}

func createManifestWithRemoteKustomize() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with remote Kustomization")
		kustomizeSpec := types.KustomizeSpec{
			URL:  "https://github.com/kyma-project/module-manager//operator/config/default?ref=main",
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"kustomize-test", false)
		deleteManifestResource(manifestObj, nil)
		return true
	}
}

func createManifestWithLocalKustomize() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with local Kustomization")
		kustomizeSpec := types.KustomizeSpec{
			Path: kustomizeLocalPath,
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"kustomize-test", false)
		deleteManifestResource(manifestObj, nil)
		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func createManifestWithInsufficientExecutePerm() func() bool {
	return func() bool {
		By("having transitioned the CR State to Error with insufficient read permissions")
		kustomizeSpec := types.KustomizeSpec{
			Path: kustomizeLocalPath,
			Type: "kustomize",
		}
		user, err := user.Current()
		Expect(err).ToNot(HaveOccurred())
		// TODO run prow pipeline without root privileges
		if user.Username == "root" {
			Skip("")
		}
		// should not be run as root user
		Expect(user.Username).ToNot(Equal("root"))
		// giving read rights only!
		Expect(os.Chmod(kustomizeLocalPath, 0o444)).ToNot(HaveOccurred())
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateError, specBytes,
			"kustomize-test", false)
		// verify permission restriction
		_, err = os.Stat(filepath.Join(kustomizeSpec.Path, util.ManifestDir))
		Expect(os.IsPermission(err)).To(BeTrue())
		// reverting permissions for deletion
		Expect(os.Chmod(kustomizeLocalPath, fs.ModePerm)).ToNot(HaveOccurred())
		deleteManifestResource(manifestObj, nil)
		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func createManifestWithInsufficientWritePermissions() func() bool {
	return func() bool {
		By("having transitioned the CR State to Error with insufficient execute permissions")
		kustomizeSpec := types.KustomizeSpec{
			Path: kustomizeLocalPath,
			Type: "kustomize",
		}
		user, err := user.Current()
		Expect(err).ToNot(HaveOccurred())
		// TODO run prow pipeline without root privileges
		if user.Username == "root" {
			Skip("")
		}
		// should not be run as root user
		Expect(user.Username).ToNot(Equal("root"))
		// giving execute rights only!
		Expect(os.Chmod(kustomizeLocalPath, 0o555)).ToNot(HaveOccurred())
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateReady, specBytes,
			"kustomize-test", false)
		// manifest was not cached due to permission issues
		_, err = os.Stat(filepath.Join(kustomizeSpec.Path, util.ManifestDir))
		Expect(os.IsNotExist(err)).To(BeTrue())
		// reverting rights
		deleteManifestResource(manifestObj, nil)
		Expect(os.Chmod(kustomizeLocalPath, fs.ModePerm)).ToNot(HaveOccurred())
		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func createManifestWithInvalidKustomize() func() bool {
	return func() bool {
		By("having transitioned the CR State to Ready with invalid Kustomization")
		kustomizeSpec := types.KustomizeSpec{
			Path: "./invalidPath",
			Type: "kustomize",
		}
		specBytes, err := json.Marshal(kustomizeSpec)
		Expect(err).ToNot(HaveOccurred())
		manifestObj := createManifestAndCheckState(v1alpha1.ManifestStateError, specBytes,
			"kustomize-test", false)
		deleteManifestResource(manifestObj, nil)
		Expect(os.RemoveAll(filepath.Join(kustomizeSpec.Path, util.ManifestDir))).ShouldNot(HaveOccurred())
		return true
	}
}

func getManifestState(key client.ObjectKey) func() v1alpha1.ManifestState {
	return func() v1alpha1.ManifestState {
		manifest := v1alpha1.Manifest{}
		err := k8sClient.Get(ctx, key, &manifest)
		if err != nil {
			return ""
		}
		return manifest.Status.State
	}
}

func getManifest(key client.ObjectKey) func() bool {
	return func() bool {
		manifest := v1alpha1.Manifest{}
		err := k8sClient.Get(ctx, key, &manifest)
		return errors.IsNotFound(err)
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

var _ = Describe("given manifest with a helm repo", Ordered, func() {
	BeforeAll(func() {
		Expect(setHelmEnv()).Should(Succeed())
	})
	BeforeEach(func() {
		Expect(os.Chmod(kustomizeLocalPath, fs.ModePerm)).ToNot(HaveOccurred())
	})

	DescribeTable("given watcherCR reconcile loop",
		func(testCaseFn func() bool) {
			Expect(testCaseFn()).To(BeTrue())
		},
		[]TableEntry{
			Entry("when two remote manifestCRs contain no install specification", createTwoRemoteManifestsWithNoInstalls()),
			Entry("when manifestCR contains invalid Kustomize specification", createManifestWithInvalidKustomize()),
			Entry("when manifestCR contains a valid helm repo", createManifestWithHelmRepo()),
			Entry("when two manifestCRs contain valid OCI Image specifications", createManifestWithOCI()),
			Entry("when two manifestCRs contain invalid OCI image specifications", createManifestWithInvalidOCI()),
			Entry("when manifestCR contains a valid local Kustomize specification", createManifestWithLocalKustomize()),
			Entry("when manifestCR contains a valid local Kustomize specification with "+
				"insufficient execute permissions", createManifestWithInsufficientExecutePerm()),
			Entry("when manifestCR contains a valid local Kustomize specification with "+
				"insufficient write permissions", createManifestWithInsufficientWritePermissions()),
			Entry("when manifestCR contains a valid remote Kustomize specification", createManifestWithRemoteKustomize()),
			// TODO write tests for pre-rendered Manifests
		})

	AfterAll(func() {
		Expect(unsetHelmEnv()).Should(Succeed())
	})
})
