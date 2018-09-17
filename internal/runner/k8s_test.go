// +build !NO_K8S

package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SentientTechnologies/studio-go-runner/internal/types"

	"github.com/ericchiang/k8s"

	core "github.com/ericchiang/k8s/apis/core/v1"
	meta "github.com/ericchiang/k8s/apis/meta/v1"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"

	"github.com/rs/xid"
)

var (
	testQErr errors.Error
)

// If we are running within a k8s cluster the the full deployment docker file
// will have been used and we can initialize the rabbitQM server using the facilities
// inside the queue test side
//
func init() {
	testQErr = InitTestQueues()
}

// This file contains a number of tests that if Kubernetes is detected as the runtime
// the test is being hosted in will be activated and used

func TestK8sConfig(t *testing.T) {
	logger := NewLogger("k8s_configmap_test")
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

// TestStates will exercise the internal changing of states within the queue processing
// of the server.  It tests the state changes without using the kubernetes side.  The k8s
// testing is done in a specific test case that just tests that component when the
// test is run within a working cluster.  To do this properly k8s should be used with a
// bundled rabbitMQ server.
//
func TestStates(t *testing.T) {

	logger := NewLogger("test_states")

	// We really need a queuing system up and running because the states and queue states that
	// are tracked in prometheus will only update in our production code when the
	// scheduler actually finds a reference to some queuing
	if testQErr != nil {
		t.Fatal(testQErr)
	}

	// send bogus updates by instrumenting the lifecycle listeners in c/r/k8s.go
	select {
	case k8SStateUpdates().Master <- K8sStateUpdate{State: types.K8sDrainAndSuspend}:
	case <-time.After(time.Second):
		t.Fatal("state change could not be sent, no master was listening")
	}

	// Retrieve prometheus counters to aws, google, and rabbit queue implementations
	timer := time.NewTicker(time.Second)

	defer func() {
		logger.Info("bailing")
		timer.Stop()

		select {
		case k8SStateUpdates().Master <- K8sStateUpdate{State: types.K8sDrainAndSuspend}:
		case <-time.After(time.Second):
			logger.Warn("state reset could not be sent, no master was listening")
		}
	}()

	pClient := NewPrometheusClient("http://localhost:9090/metrics")

	logger.Info("Waiting for the timer for the Fetch")
	select {
	case <-timer.C:
		logger.Info("fired")
		err := pClient.Fetch("")
		if err != nil {
			t.Fatal(err)
		}
	}

	// Consider someway to combining some elements of the three of them
	// Consider splitting out the lifecycle listeners channel side into a channel pattern library
	// Send the biogus signal
	// see what the prometheus counters do
	// done

	logger.Info("test_states done")
}
