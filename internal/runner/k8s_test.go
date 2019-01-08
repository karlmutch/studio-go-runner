package runner

import (
	"context"
	"testing"
	"time"

	"github.com/leaf-ai/studio-go-runner/internal/types"
	"github.com/leaf-ai/studio-go-runner/pkg/studio"

	"github.com/ericchiang/k8s"
	core "github.com/ericchiang/k8s/apis/core/v1"
	meta "github.com/ericchiang/k8s/apis/meta/v1"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"

	"github.com/rs/xid"
)

// This file contains a number of tests that if Kubernetes is detected as the runtime
// the test is being hosted in will be activated and used.  This is a unit test
// that exercises a listener specifically constructed for the purpose of catching
// changes to a configmap.
//
func TestK8sConfigUnit(t *testing.T) {
	logger := studio.NewLogger("k8s_configmap_test")

	if !*useK8s {
		t.Skip("no Kubernetes cluster present for testing")
	}

	if err := IsAliveK8s(); err != nil {
		t.Fatal(err)
	}

	client, errGo := k8s.NewInClusterClient()
	if errGo != nil {
		t.Fatal(errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}

	namespace := "default"
	name := "test-" + xid.New().String()

	configMap := &core.ConfigMap{
		Metadata: &meta.ObjectMeta{
			Name:      k8s.String(name),
			Namespace: k8s.String(namespace),
		},
		Data: map[string]string{"STATE": types.K8sRunning.String()},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Establish a listener for the API under test
	updateC := make(chan K8sStateUpdate, 1)
	errC := make(chan errors.Error, 1)

	// Register a listener for the newly created map
	if err := ListenK8s(ctx, namespace, name, "", updateC, errC); err != nil {
		t.Fatal(err)
	}

	// Go and create a k8s config map that we can use for testing purposes
	if errGo = client.Create(ctx, configMap); errGo != nil {
		t.Fatal(errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}

	// Now see if we get the state change with "Running"
	func() {
		for {
			select {
			case <-ctx.Done():
				t.Fatal(errors.New("timeout waiting for k8s configmap to change state").With("stack", stack.Trace().TrimRuntime()))
			case state := <-updateC:
				if state.Name == name && state.State == types.K8sRunning {
					return
				}
			}
		}
	}()

	// Change the map and see if things get notified
	configMap.Data["STATE"] = types.K8sDrainAndSuspend.String()

	// Go and create a k8s config map that we can use for testing purposes
	if errGo = client.Update(ctx, configMap); errGo != nil {
		t.Fatal(errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}

	// Now see if we get the state change with "Running"
	func() {
		for {
			select {
			case <-ctx.Done():
				t.Fatal(errors.New("timeout waiting for k8s configmap to change state").With("stack", stack.Trace().TrimRuntime()))
			case state := <-updateC:
				if state.Name == name && state.State == types.K8sDrainAndSuspend {
					return
				}
			}
		}
	}()

	// Cleanup after ourselves
	if errGo = client.Delete(ctx, configMap); errGo != nil {
		t.Fatal(errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}

	logger.Info("TestK8sConfig completed")
}
