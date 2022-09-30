package smoke

import (
	"context"
	"flag"
	"log"
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

var (
	TestEnv                  env.Environment
	ControllerDeploymentName = "module-manager-controller-manager"
	KCP = "kcp-system"
)

func TestMain(m *testing.M) {
	log.Println("setting up test environment from flags")
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(err)
	}

	flag.Parse()

	log.Println("creating test environment")
	TestEnv = env.NewWithConfig(cfg)

	os.Exit(TestEnv.Run(m))
}

func TestControllerManagerSpinsUp(t *testing.T) {
	depFeature := features.New("appsv1/deployment/controller-manager").
		WithLabel("app.kubernetes.io/component", "module-manager.kyma-project.io").
		WithLabel("test-type.kyma-project.io", "smoke").
		Assess("exists", controllerExists).
		Assess("available", controllerAvailable).Feature()

	TestEnv.Test(t, depFeature)
}

func controllerExists(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	dep := ControllerManagerDeployment(KCP, ControllerDeploymentName)
	// wait for the deployment to finish becoming available
	err = wait.For(
		conditions.New(client.Resources()).ResourcesFound(&appsv1.DeploymentList{Items: []appsv1.Deployment{dep}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func controllerAvailable(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	dep := ControllerManagerDeployment(KCP, ControllerDeploymentName)
	// wait for the deployment to finish becoming available
	err = wait.For(
		conditions.New(client.Resources()).DeploymentConditionMatch(
			dep.DeepCopy(),
			appsv1.DeploymentAvailable, corev1.ConditionTrue,
		),
		wait.WithTimeout(time.Minute*3),
	)

	pods := corev1.PodList{}
	_ = client.Resources(KCP).List(ctx, &pods)
	for _, pod := range pods.Items {
		if marshal, err := yaml.Marshal(&pod.Status); err == nil {
			t.Logf("Pod Status For %s/%s\n%s", pod.Namespace, pod.Name, marshal)
		}
	}

	logDeployStatus(t, ctx, client, dep)

	if err != nil {
		t.Fatal(err)
	}

	return ctx
}

func logDeployStatus(t *testing.T, ctx context.Context, client klient.Client, dep appsv1.Deployment) {
	errCheckCtx, cancelErrCheck := context.WithTimeout(ctx, 5*time.Second)
	defer cancelErrCheck()
	if err := client.Resources().Get(errCheckCtx, dep.Name, dep.Namespace, &dep); err != nil {
		t.Error(err)
	}
	if marshal, err := yaml.Marshal(&dep.Status); err == nil {
		t.Logf("%s", marshal)
	}
}

func ControllerManagerDeployment(namespace string, name string) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/component": "module-manager.kyma-project.io"}},
	}
}
