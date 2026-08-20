package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	appsv1alpha1 "brewlet-operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest harness shared by the *_envtest_test.go reconcile tests.
//
// These tests exercise the real Reconcile() loops against a genuine
// kube-apiserver + etcd (controller-runtime's envtest), with no kubelet — the
// fast "middle" layer between the pure builder unit tests in this package and
// heavyweight end-to-end tiers. They load the shipped CRDs from deploy/ so
// schema/CEL drift is caught here too.
//
// envtest needs the control-plane binaries (kube-apiserver, etcd, kubectl).
// setup-envtest downloads them and exports KUBEBUILDER_ASSETS. Mirroring the
// e2e suite's graceful-degradation philosophy (a tier whose prerequisites are
// missing SKIPs, it does not fail), when KUBEBUILDER_ASSETS is unset every
// envtest-backed test SKIPs, so `go test ./...` stays green on a bare machine.
// CI wires setup-envtest (see .github/workflows/ci.yml) so the suite runs there.
var (
	testEnv       *envtest.Environment
	testRestCfg   *rest.Config
	testScheme    *runtime.Scheme
	envtestReason string // non-empty means: skip, envtest not available
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// No control-plane binaries advertised: run the non-envtest tests in
		// this package and let the envtest-backed ones self-skip.
		envtestReason = "KUBEBUILDER_ASSETS not set — run `setup-envtest use` and export it (CI does this)"
		os.Exit(m.Run())
	}

	testScheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(appsv1alpha1.AddToScheme(testScheme))
	utilruntime.Must(nodev1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		// The shipped CRDs, so envtest validates against exactly what we deploy
		// (including the status subresource and the autoscaling CEL rules).
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "javaapplication-crd.yaml"),
			filepath.Join("..", "..", "deploy", "nodeprofile-crd.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		// KUBEBUILDER_ASSETS was set but the control plane failed to start —
		// that is a real misconfiguration (e.g. wrong asset path), so fail
		// loudly rather than silently skipping in CI.
		fmt.Fprintf(os.Stderr, "failed to start envtest control plane: %v\n", err)
		os.Exit(1)
	}
	testRestCfg = cfg

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest control plane: %v\n", err)
	}
	os.Exit(code)
}

// requireEnvtest skips the calling test when the envtest control plane is not
// available (KUBEBUILDER_ASSETS unset). It returns a client bound to the live
// apiserver otherwise.
func requireEnvtest(t *testing.T) client.Client {
	t.Helper()
	if envtestReason != "" {
		t.Skipf("envtest unavailable: %s", envtestReason)
	}
	c, err := client.New(testRestCfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("creating envtest client: %v", err)
	}
	return c
}

// testContext returns a context with a generous deadline for a single test.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
